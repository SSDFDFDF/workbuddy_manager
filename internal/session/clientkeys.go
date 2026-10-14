// clientkeys.go 常用 AI 客户端的会话标识提取（粘性键扩展键位）。
//
// 除 conversation_id 族之外，主流 CLI/客户端还会把会话标识散落在自定义头与
// body 的非标准位置。本文件按实抓口径把它们接进粘性键链：
//
//   - HTTP 头层（HeaderSessionKey，高信任度在前）：
//     1. X-Claude-Code-Session-Id（Claude Code CLI）     → "claude:"
//     2. Session-Id / Session_id（Codex CLI）            → "codex:"
//     3. X-Http-Session-Id（Antigravity CLI）            → "agy:"
//     4. X-Session-ID（通用）                            → "hdr:"
//     5. X-Session-Affinity（OpenCode，ses_... 格式）     → "affinity:"
//     6. X-Slot-Session-Id（pi 客户端）                  → "slot:"
//     7. X-Task-Id（Roo Code / Cline 任务委派）           → "task:"
//     8. X-Thread-Id / Thread-Id（OpenAI thread/通用）   → "thread:"
//     9. X-Client-Request-Id（PI / 通用请求标识）         → "clientreq:"
//   - body 层（BodyKey，一次解析完成整个回退链）：
//     1. metadata.user_id 的 Claude Code session 段（格式闸，两种形态） → "claude:"
//     2. thread_id 族（顶层/metadata/extra_body）        → "thread:"
//     3. session_id 族（顶层/metadata/extra_body）       → "session:"
//     4. task_id / action_id 族                          → "task:"
//     5. conversation.id（对象）/ conversation（字符串）  → "conv:"
//     6. prompt_cache_key（裸值，历史键格式——断链前的 ExtractKey 第 5 键
//     就是裸值入键，保持存量绑定兼容）；
//     7. 内容派生键（system + 首条 user 签名哈希，带 user_id 抑制闸）。
//
// 命名空间前缀的作用：不同来源的标识值即便字面相同也不互撞（client A 的
// conversation_id "abc" 与 client B 的 session_id "abc" 各归各的绑定）；同一来源
// 的等价信号（X-Claude-Code-Session-Id 头与 metadata.user_id 的 session 段）
// 共享同一前缀，天然归并为同一粘性会话。
//
// 本网关原生键 conversation_id（forwarding.Parse → peek.ConversationID）**不加前缀**
// ——它是自始以来的粘性键格式，加前缀会让存量绑定（Redis 镜像 + 内存）在升级后
// 全部 miss；prompt_cache_key 同理保持裸值。其余新键位都带前缀，天然避开原生键
// 的裸值命名空间。
//
// 提取值统一过 normalizeSessionID：去首尾空白、拒绝控制字符、超长（>256 字节）
// 折叠为「前 191 字节 + '#' + SHA-256 64 hex」，防客户端超长标识把绑定表 key 撑爆。
//
// 语义隔离契约不变：所有提取值只作网关侧粘性键，绝不进入上游
// X-Conversation-ID / 缓存键材料。metadata.user_id 的反垄断抑制闸同样不变——
// 非 Claude 格式的 user_id 不参与粘性（格式闸），且仍会抑制内容派生
// （P1-anti-monopoly 契约）。
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// 命名空间前缀：为不同来源的会话标识隔离键空间。
const (
	prefixClaude    = "claude:"
	prefixCodex     = "codex:"
	prefixAgy       = "agy:"
	prefixHeader    = "hdr:"
	prefixAffinity  = "affinity:"
	prefixSlot      = "slot:"
	prefixTask      = "task:"
	prefixThread    = "thread:"
	prefixClientReq = "clientreq:"
	prefixSession   = "session:"
	prefixConv      = "conv:"
)

// maxSessionIDBytes 粘性键裸值的长度上限。
const maxSessionIDBytes = 256

// boundedPrefixLen 超长值折叠时保留的明文前缀长度：256 - 1('#') - 64(hex) = 191。
const boundedPrefixLen = maxSessionIDBytes - 1 - 64

// claudeUserSessionPattern Claude Code 的 metadata.user_id 旧形态：
// "user_{account_hash}_session_{uuid}"。捕获组 1 = session uuid。
var claudeUserSessionPattern = regexp.MustCompile(`^user_[A-Za-z0-9_-]+_session_([0-9a-fA-F-]{8,64})$`)

// normalizeSessionID 验证并归一显式会话标识：去首尾空白、拒绝控制字符、
// 超长折叠（保留可读前缀 + '#' + 全量 SHA-256，UTF-8 边界回退）。
// 非法（空/控制字符）返回 ""。
func normalizeSessionID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	for _, r := range raw {
		if unicode.IsControl(r) {
			return ""
		}
	}
	if len(raw) <= maxSessionIDBytes {
		return raw
	}
	sum := sha256.Sum256([]byte(raw))
	prefix := raw
	if len(prefix) > boundedPrefixLen {
		prefix = prefix[:boundedPrefixLen]
	}
	for !utf8.ValidString(prefix) && len(prefix) > 0 {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix + "#" + hex.EncodeToString(sum[:])
}

// headerValue 按候选名列表取首个非空头值。Get 之外再做 EqualFold 全表扫描：
// net/http 对下划线头名（Session_id）不做连字符合并，且中间件可能改写头名形态。
func headerValue(h http.Header, names ...string) string {
	if h == nil {
		return ""
	}
	for _, name := range names {
		if v := normalizeSessionID(h.Get(name)); v != "" {
			return v
		}
	}
	for _, name := range names {
		for key, vals := range h {
			if !strings.EqualFold(key, name) || len(vals) == 0 {
				continue
			}
			if v := normalizeSessionID(vals[0]); v != "" {
				return v
			}
		}
	}
	return ""
}

// HeaderSessionKey 按实抓优先级提取客户端自定义会话头（HTTP 头层），
// 返回带命名空间前缀的键；全部缺失返回 ""。
// 优先级即上方包注释的头层顺序（claude → codex → agy → hdr → affinity →
// slot → task → thread → clientreq）。
func HeaderSessionKey(h http.Header) string {
	if v := headerValue(h, "X-Claude-Code-Session-Id"); v != "" {
		return prefixClaude + v
	}
	if v := headerValue(h, "Session-Id", "Session_id"); v != "" {
		return prefixCodex + v
	}
	if v := headerValue(h, "X-Http-Session-Id"); v != "" {
		return prefixAgy + v
	}
	if v := headerValue(h, "X-Session-ID"); v != "" {
		return prefixHeader + v
	}
	if v := headerValue(h, "X-Session-Affinity"); v != "" {
		return prefixAffinity + v
	}
	if v := headerValue(h, "X-Slot-Session-Id"); v != "" {
		return prefixSlot + v
	}
	if v := headerValue(h, "X-Task-Id", "X-Task_ID"); v != "" {
		return prefixTask + v
	}
	if v := headerValue(h, "X-Thread-Id", "Thread-Id", "Thread_id"); v != "" {
		return prefixThread + v
	}
	if v := headerValue(h, "X-Client-Request-Id"); v != "" {
		return prefixClientReq + v
	}
	return ""
}

// claudeUserIDSession 从 metadata.user_id 值提取 Claude Code 会话段。
// 仅认两种形态（格式闸）：
//   - 旧格式 "user_{hash}_session_{uuid}"（正则）；
//   - JSON 形态 {"session_id":"...", ...}——真实 Claude Code 的 identity JSON 还带
//     device_id/account_uuid/parent_session_id 等键，只取 session_id 字段，
//     其余键忽略。
//
// 其余（裸 user_id、API key 等）返回 ""——反垄断契约保持：非会话语义的 user_id
// 不参与粘性。
func claudeUserIDSession(v string) string {
	if v == "" {
		return ""
	}
	if m := claudeUserSessionPattern.FindStringSubmatch(v); m != nil {
		return m[1]
	}
	if strings.HasPrefix(strings.TrimSpace(v), "{") {
		var obj map[string]any
		if json.Unmarshal([]byte(v), &obj) == nil {
			if s, ok := obj["session_id"].(string); ok {
				return s
			}
		}
	}
	return ""
}

// lookupPath 取嵌套路径的字符串值（"metadata.session_id" / "extra_body.thread_id"），
// 途经节点非对象或叶非字符串返回 ""；值过 normalizeSessionID 归一。
func lookupPath(obj map[string]any, path string) string {
	cur := obj
	// 逐段下钻：cur 持当前层对象，path 保存剩余路径（每轮 Cut 后重新赋值——
	// 若每轮都 Cut 完整 path，第二层永远查不到）。
	for len(path) > 0 {
		var key string
		key, path, _ = strings.Cut(path, ".")
		v, ok := cur[key]
		if !ok || v == nil {
			return ""
		}
		if path == "" {
			return normalizeSessionID(strOrEmpty(v))
		}
		if cur, ok = v.(map[string]any); !ok {
			return ""
		}
	}
	return ""
}

func firstPath(obj map[string]any, paths ...string) string {
	for _, p := range paths {
		if v := lookupPath(obj, p); v != "" {
			return v
		}
	}
	return ""
}

// bodySessionKeyObj 在已解析的 body 对象上提取客户端非标准会话标识（键位链见
// 包注释 body 层 1-5 级），返回带命名空间前缀的键；无命中返回 ""。
func bodySessionKeyObj(obj map[string]any) string {
	// 1. Claude Code 会话：metadata.user_id（格式闸，两种形态）。
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if u, ok := meta["user_id"].(string); ok {
			if s := normalizeSessionID(claudeUserIDSession(u)); s != "" {
				return prefixClaude + s
			}
		}
	}
	// 2. thread_id 族（OpenAI thread / Codex）。
	if v := firstPath(obj,
		"thread_id", "threadId", "threadID",
		"metadata.thread_id", "metadata.threadId",
		"extra_body.thread_id", "extra_body.threadId",
	); v != "" {
		return prefixThread + v
	}
	// 3. session_id 族（通用客户端）。
	if v := firstPath(obj,
		"session_id", "sessionId", "sessionID",
		"child_session_id", "childSessionId",
		"metadata.session_id", "metadata.sessionId", "metadata.sessionID",
		"metadata.child_session_id",
		"extra_body.session_id", "extra_body.sessionId",
	); v != "" {
		return prefixSession + v
	}
	// 4. task_id / action_id 族（Roo Code / Cline / OpenHands 任务委派）。
	if v := firstPath(obj,
		"task_id", "taskId", "taskID",
		"action_id", "actionId", "actionID",
		"metadata.task_id", "metadata.taskId", "metadata.taskID",
		"metadata.action_id", "metadata.actionId",
		"extra_body.task_id", "extra_body.taskId",
	); v != "" {
		return prefixTask + v
	}
	// 5. conversation 对象 id / 字符串。
	switch c := obj["conversation"].(type) {
	case map[string]any:
		if id := normalizeSessionID(strOrEmpty(c["id"])); id != "" {
			return prefixConv + id
		}
	case string:
		if id := normalizeSessionID(c); id != "" {
			return prefixConv + id
		}
	}
	return ""
}

// BodyKey 是 handler 在 conversation 维度键与客户端会话头都未命中后的 body 层
// 提取入口，**一次解析**依次完成：非标准会话标识键位链 → prompt_cache_key →
// 内容派生键。body 是入站原始字节（跨协议请求同样是原始协议 JSON，
// /v1/messages 的 metadata.user_id 也在这里命中）。
//
// prompt_cache_key 由调用方传入（forwarding.Parse 已校验的原值）并保持裸值入键
// ——断链前 ExtractKey 第 5 键就是裸值，保持存量绑定兼容；优先级在非标准 body
// 键之后。
//
// 派生键的 user_id 抑制闸不变：body 携带 metadata.user_id / 顶层 user_id 时不
// 派生（P1-anti-monopoly 契约——user 维度粒度过粗）。注意抑制闸只作用于派生，
// 不影响 Claude 格式 user_id 的显式提取（那由键位链第 1 级负责，优先级更高）。
func BodyKey(body []byte, promptCacheKey string) string {
	if len(body) > 0 {
		var obj map[string]any
		if err := json.Unmarshal(body, &obj); err == nil {
			return BodyKeyObject(obj, promptCacheKey)
		}
	}
	// body 为空 / 解析失败：prompt_cache_key 仍可用（forwarding 层已校验）。
	return promptCacheKey
}

// BodyKeyObject 在未改变会话材料的原协议解码树上提取键，不修改 obj。
// 跨协议必须传原始 Source，不能传翻译后的 Chat（字段与派生材料不同）。
func BodyKeyObject(obj map[string]any, promptCacheKey string) string {
	if k := ExplicitBodyKeyObject(obj, promptCacheKey); k != "" {
		return k
	}
	if hasUserIDKey(obj) {
		return ""
	}
	return deriveKey(obj)
}

// ExplicitBodyKeyObject returns only client-provided session/cache identifiers;
// unlike BodyKeyObject it never falls back to a content-derived sticky key.
func ExplicitBodyKeyObject(obj map[string]any, promptCacheKey string) string {
	if k := bodySessionKeyObj(obj); k != "" {
		return k
	}
	return promptCacheKey
}
