// queue.go 上游「模型容量队列」（业务码 6020/6021/6022）的识别与信息解析。
//
// 为什么需要它：上游对热门/新模型会先入队再服务，官方桌面端为此实现了整套队列
// 交互（5.6.2 客户端 bundle 的 capacity-queue：/v2/chat/queue/status|cancel、
// QUEUE_ERROR_CODES = {WAITING:6020, FULL:6021, USER_LIMIT:6022}、retry_after
// 归一、按 queue_position 自适应轮询）。网关此前**完全不认**这三个码：
//   - HTTP 202（上游"已受理/排队"形态）在 chat 出站被当成 502 ErrServer
//     （`upstream_queue_unsupported`），客户端拿到一个不存在上游原因的错误；
//   - 业务信封里的 6020/6021/6022 落通用 4xx 兜底 ErrClient（"请求有问题"），
//     排队前缀语义与可重试语义全丢；6022 的 msg 常含 "limit"，还可能被限流词表
//     误伤成 ErrSoftRate → 给健康账号记一次 429 软冷却（纯误罚）。
//
// 判定纪律：只认**结构化 code**（`code` / `bizCode` / `biz_code` 三键，桌面端
// 用 bizCode，HTTP 信封用 code），绝不做文案猜测——三个码没有稳定文案，而
// 6022 的文案反而容易撞上限流/额度词表。
package upstream

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 队列业务码（与官方桌面端 capacity-queue-constants.ts 逐字一致）。
const (
	QueueCodeWaiting   = "6020" // 已入队，等待放行（带 retry_after / queue_position）
	QueueCodeFull      = "6021" // 队列已满（全局容量，换号无益）
	QueueCodeUserLimit = "6022" // 该用户的模型级队列上限（账号维度）
)

// queueRetryAfterSanity 队列等待时长上限（与 retryAfterSanity 同口径：超过视为
// 上游异常值，丢弃而不是照抄给客户端）。下限 1s 由归一函数保证。
const queueRetryAfterSanity = retryAfterSanity

// QueueInfo 一次队列答复里可用的排队信息。字段缺失一律零值（不编造）。
type QueueInfo struct {
	// Code 命中的队列业务码（QueueCodeWaiting / Full / UserLimit）。
	Code string
	// RetryAfter data.retry_after 归一后的等待时长；0 = 上游未给/不可用。
	RetryAfter time.Duration
	// Position data.queue_position（等待中时的队伍位置）；0 = 未给。
	Position int64
	// Size data.queue_size；0 = 未给。
	Size int64
}

// IsModelQueuedCode 报告 code 是否为已知的队列业务码。
func IsModelQueuedCode(code string) bool {
	switch strings.TrimSpace(code) {
	case QueueCodeWaiting, QueueCodeFull, QueueCodeUserLimit:
		return true
	}
	return false
}

// QueueCode 从上游答复（HTTP body 或 SSE error 帧 payload）里提取队列业务码。
// 未命中返回空串。调用方按「非空即排队」判定。
//
// 先做一次纯子串预检（%13 GB/s，绝大多数错误体零分配短路），命中才解析 JSON。
func QueueCode(body string) string {
	if !containsQueueCode(body) {
		return ""
	}
	info, ok := ParseQueueInfo(body)
	if !ok {
		return ""
	}
	return info.Code
}

// containsQueueCode 三码子串预检。JSON 里数字必以字面量出现（6020 / "6020" 皆含），
// 因此子串命中是必要条件；未命中即可安全短路，不需要更精确的判定。
func containsQueueCode(body string) bool {
	return strings.Contains(body, QueueCodeWaiting) ||
		strings.Contains(body, QueueCodeFull) ||
		strings.Contains(body, QueueCodeUserLimit)
}

// BusinessCode 从上游答复里提取业务码（`code` / `bizCode` / `biz_code`），
// 供请求归档回答“上游到底回了什么码”（队列 6020/6021/6022、11102、6004 …）。
//
// 取**最浅**的那个（信封码是权威码，内层可能是局部错误码）；非 JSON body 返回
// 空串（不扫文本：文案里出现的数字可能来自 token 数、请求 ID 等，猜不如不猜）。
// 返回值限长 32 字符，防病态 body 把归档字段撑爆。
func BusinessCode(body string) string {
	// 预检用 "ode"：三键大小写形态不一（code / Code / biz_code 都含该子串），
	// 用 "code" 会漏掉 bizCode/biz_code。
	if !strings.Contains(body, "ode") {
		return ""
	}
	var root any
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		return ""
	}
	best := ""
	bestDepth := -1
	var walk func(node any, depth int)
	walk = func(node any, depth int) {
		switch n := node.(type) {
		case map[string]any:
			if best == "" || depth < bestDepth {
				for _, key := range [...]string{"code", "bizCode", "biz_code"} {
					v, ok := n[key]
					if !ok || v == nil {
						continue
					}
					s := strings.TrimSpace(fmt.Sprint(v))
					if s == "" || s == "0" {
						continue
					}
					if best == "" || depth < bestDepth {
						best, bestDepth = s, depth
					}
					break
				}
			}
			for _, child := range n {
				walk(child, depth+1)
			}
		case []any:
			for _, child := range n {
				walk(child, depth+1)
			}
		}
	}
	walk(root, 0)
	if len(best) > 32 {
		best = best[:32]
	}
	return best
}

// ParseQueueInfo 解析上游队列答复（信封或 SSE 帧 payload 两种形态）。
// 未含已知队列码时返回 ok=false（此时不视为排队）。
//
// 形状容错：真实语料同时存在 {"code":6020,"data":{...}}（HTTP 信封）与
// {"data":{"data":{...}}}（队列数据的双层包裹）两形，字段一律「更深的赢」
// ——外层信封可能带同名但语义不同的字段。
func ParseQueueInfo(body string) (QueueInfo, bool) {
	if strings.TrimSpace(body) == "" {
		return QueueInfo{}, false
	}
	var root any
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		return QueueInfo{}, false
	}
	info := QueueInfo{RetryAfter: -1} // 内部哨兵：区分「未出现」与「出现但为 0」
	posDepth, sizeDepth, retryDepth := -1, -1, -1
	var walk func(node any, depth int)
	walk = func(node any, depth int) {
		switch n := node.(type) {
		case map[string]any:
			if info.Code == "" {
				for _, key := range [...]string{"code", "bizCode", "biz_code"} {
					v, ok := n[key]
					if !ok {
						continue
					}
					if c := strings.TrimSpace(fmt.Sprint(v)); IsModelQueuedCode(c) {
						info.Code = c
						break
					}
				}
			}
			if v, ok := queueNumber(n["queue_position"]); ok && depth > posDepth {
				info.Position, posDepth = v, depth
			}
			if v, ok := queueNumber(n["queue_size"]); ok && depth > sizeDepth {
				info.Size, sizeDepth = v, depth
			}
			if v, ok := queueNumber(n["retry_after"]); ok && depth > retryDepth {
				info.RetryAfter, retryDepth = queueRetryAfterOf(v), depth
			}
			for _, child := range n {
				walk(child, depth+1)
			}
		case []any:
			for _, child := range n {
				walk(child, depth+1)
			}
		}
	}
	walk(root, 0)
	if info.Code == "" {
		return QueueInfo{}, false
	}
	if info.RetryAfter < 0 {
		info.RetryAfter = 0
	}
	return info, true
}

// QueueRetryAfter 便捷入口：只取等待时长（未命中/未给 → 0）。
func QueueRetryAfter(body string) time.Duration {
	info, ok := ParseQueueInfo(body)
	if !ok {
		return 0
	}
	return info.RetryAfter
}

// queueRetryAfterOf 把 data.retry_after 归一为时长。
//
// 口径与官方桌面端 normalizeRetryAfterMs 一致：<1000 视为秒、>=1000 视为毫秒
// （实测该字段两种口径都出现过）；夹在 [1s, 2h]——异常值丢弃为 0（宁缺毋滥：
// 把不合理的等待时长原样回给客户端，等于让客户端等到天荒地老）。
func queueRetryAfterOf(v int64) time.Duration {
	if v <= 0 {
		return 0
	}
	d := time.Duration(v) * time.Second
	if v >= 1000 {
		d = time.Duration(v) * time.Millisecond
	}
	if d < time.Second || d > queueRetryAfterSanity {
		return 0
	}
	return d
}

// queueNumber 把 JSON 数字/数字字符串读成 int64（负数与非数字一律不认）。
func queueNumber(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		if n < 0 {
			return 0, false
		}
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil || i < 0 {
			return 0, false
		}
		return i, true
	case string:
		var i int64
		if _, err := fmt.Sscan(strings.TrimSpace(n), &i); err != nil || i < 0 {
			return 0, false
		}
		return i, true
	}
	return 0, false
}
