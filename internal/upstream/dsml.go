// dsml.go 「模型原生工具调用标记」还原层：上游在请求未声明 tools 时把工具调用吐成
// 原生标记文本落进 content，这里在响应出站前把它还原成结构化 tool_calls。
//
// 背景：网关背后的 WorkBuddy 上游由 DeepSeek 系模型出结果，它的工具调用原生语法
// 不是 OpenAI 的 JSON tool_calls，而是一段带全角竖线分隔符的标记文本：
//
//	｜｜DSML｜｜ calls>
//	｜｜DSML｜｜ invoke name="exec_command">
//	｜｜DSML｜｜ parameter name="cmd" string="true">ls -la｜｜DSML｜｜ parameter>
//	</｜｜DSML｜｜ invoke>
//	</｜｜DSML｜｜ calls>
//
// （上面用全角竖线直书；真实分隔符是两个 U+FF5C，见 markupPipe 常量。）
//
// 上游只在请求**声明了 tools** 时才把这段标记解析成结构化 tool_calls。客户端这一
// 回合没声明 tools（一次性 exec 探测、复核回合、或 tools 声明在中转环节丢失）时，
// 模型仍然想调工具，于是这段标记以纯文本落进 content。下游没有任何一层认识它：
// Codex 只执行 Responses API 的结构化 function_call 条目，把标记当普通正文写进
// 会话历史，回合直接结束、工具一次都没跑。
//
// 与 FORK_CHANGES §1「转发契约（严格、不改写）」的关系：本层是 §15 式「显式可见的
// 近似变换」，不是静默改写——命中时调用方必须写进请求归档的改写列（面板 LogsView
// 的改写标签）并打一行日志。模型名猜测式补丁仍被禁止：本层不改请求参数、不搬移
// system、不伪造模型没发出的内容，只把**模型自己发出的**标记语法还原成结构化调用。
//
// 判定分两级，因为「客户端声明了哪些工具」在链路上并不可靠——实测最常见的泄漏
// 场景恰恰是 tools 声明缺失，此时请求里没有名单可查：
//
//	严格判定（名单非空）：块内每个 invoke 的工具名都必须在名单里；
//	弱判定（名单为空）：块结构完美闭合、块内不夹带正文，且工具名形如合法标识符。
//
// 两级判定共同的硬性前提（任一不满足即原文透出）：
//
//   - 标记块完整闭合，块内除 invoke/parameter 与空白外没有夹带其他正文；
//   - 本 choice 上游没有给出任何结构化 tool_calls（真实调用一律优先，还原层让位）。
//
// 任何拿不准的分支都走回吐路径：本层绝不吞字节。
package upstream

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"workbuddy_manager/internal/jsondoc"
)

// markupPipe 是模型原生工具调用标记的分隔符：两个全角竖线 U+FF5C。
const markupPipe = "｜｜"

// 标记块与内层元素的起止标记。结束标记一律比同名的开始标记多一个 '/'，
// 因此 markupInvokeRe / markupParamRe 里「< 开头」的匹配不可能落到结束标记上。
const (
	markupOpen        = "<" + markupPipe + "DSML" + markupPipe + " calls>"
	markupClose       = "</" + markupPipe + "DSML" + markupPipe + " calls>"
	markupInvokeClose = "</" + markupPipe + "DSML" + markupPipe + " invoke>"
	markupParamClose  = "</" + markupPipe + "DSML" + markupPipe + " parameter>"
)

// markupTagPrefix 是开始标记的公共前缀（已转义，供下面两个正则复用）。
var markupTagPrefix = regexp.QuoteMeta("<" + markupPipe + "DSML" + markupPipe)

var (
	// markupInvokeRe 匹配 invoke 头：<PIPE>DSML<PIPE> invoke name="NAME">。
	markupInvokeRe = regexp.MustCompile(markupTagPrefix + `\s+invoke\s+name="([^"]*)"\s*>`)
	// markupParamRe 匹配 parameter 头：<PIPE>DSML<PIPE> parameter name="NAME" string="true">，
	// string 属性可选（缺省按 JSON 解析取值）。
	markupParamRe = regexp.MustCompile(markupTagPrefix + `\s+parameter\s+name="([^"]*)"(?:\s+string="(true|false)")?\s*>`)
)

// markupToolNameRe 是工具名的合理形态：各家 SDK 的工具名一律是这种标识符。
// 弱判定（客户端没给出名单）时用它兜底，挡住模型在正文里「讨论」这段语法时
// 写出的 <tool_name>、NAME 之类占位符。
var markupToolNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:-]{0,63}$`)

// markupMaxBlockBytes 单次标记块的累积上限。正常块从几百字节（一条 ls）到几十 KB
// （一段 apply_patch）。超过上限仍未闭合 → 判定「这不是标记」，立即原文回吐并
// 退出块态，避免无限缓冲。
const markupMaxBlockBytes = 256 * 1024

// MarkupCall 是一次从正文里还原出来的工具调用。
type MarkupCall struct {
	Name      string
	Arguments string // JSON 对象文本
}

// MarkupRepair 是一次请求的标记还原配置 + 观测计数器。每次出站尝试（轮转循环里
// 每一跳）各自新建一个：还原状态机按流持有，计数器跨 choice 聚合，响应结束由
// 调用方读取写日志与请求归档。非并发安全：一条响应流一个对象。
type MarkupRepair struct {
	allowed   map[string]bool
	allowWeak bool
	converted int
	seen      int
	rejected  int
}

// NewMarkupRepair 按客户端声明的工具名构造还原器。
//
// allowed 为请求体里提取到的工具名集合（可能为空——tools 声明缺失是泄漏的常态）。
// allowUnknown 为 true 时启用弱判定：名单为空也尝试还原，改由「块结构完美 + 工具名
// 形如标识符」把关。两者都空/为 false → 还原器整体禁用，Feed 恒等透传。
func NewMarkupRepair(allowed map[string]bool, allowUnknown bool) *MarkupRepair {
	r := &MarkupRepair{allowWeak: allowUnknown}
	if len(allowed) > 0 {
		r.allowed = make(map[string]bool, len(allowed))
		for k := range allowed {
			r.allowed[k] = true
		}
	}
	return r
}

// Enabled 报告还原器是否启用。
func (r *MarkupRepair) Enabled() bool {
	return r != nil && (len(r.allowed) > 0 || r.allowWeak)
}

// Converted 返回本次响应里已还原的工具调用总数（观测用）。
func (r *MarkupRepair) Converted() int {
	if r == nil {
		return 0
	}
	return r.converted
}

// Seen 返回本次响应里识别到的标记块数（含被拒绝的）。
func (r *MarkupRepair) Seen() int {
	if r == nil {
		return 0
	}
	return r.seen
}

// Rejected 返回识别到但未还原的块数。Seen 与 Converted 的差额即此值；
// 单独暴露是为了排障时能一眼区分「没识别到」与「识别到但判定不通过」。
func (r *MarkupRepair) Rejected() int {
	if r == nil {
		return 0
	}
	return r.rejected
}

// markupMachine 是单个 choice 上的还原状态机（跨帧持有缓冲）。
// 零值可用；计数写入共享的 parent。
type markupMachine struct {
	parent  *MarkupRepair
	hold    string // 已确认可能属于标记、但还不足以判定的尾部缓冲
	inBlock bool   // 已消费掉 markupOpen，正在块内部累积
}

func newMarkupMachine(parent *MarkupRepair) *markupMachine {
	return &markupMachine{parent: parent}
}

// Feed 吃进一段正文增量，返回「应当作为正文透出的部分」与「还原出来的工具调用」。
// 调用方必须把返回的正文按原顺序写出，并把返回的调用按出现顺序转成 tool_call。
//
// 不变量：所有 Feed 的入参拼接后，等于「所有返回的正文 + Flush() + 被成功转换的
// 标记块原文」。即除了被转换掉的标记块本身，一个字节都不会丢。
func (m *markupMachine) Feed(text string) (string, []MarkupCall) {
	s := m.hold + text
	m.hold = ""
	var emit strings.Builder
	var calls []MarkupCall
	for {
		if !m.inBlock {
			i := markupStartCandidate(s)
			if i < 0 {
				emit.WriteString(s)
				break
			}
			if len(s)-i < len(markupOpen) {
				// 起点被截断在增量边界（上游把标记切成了两片）：留到下一片再判定。
				emit.WriteString(s[:i])
				m.hold = s[i:]
				break
			}
			emit.WriteString(s[:i])
			s = s[i+len(markupOpen):]
			m.inBlock = true
			continue
		}
		k := strings.Index(s, markupClose)
		if k < 0 {
			if len(s) > markupMaxBlockBytes {
				// 超长未闭合：不是标记，原文回吐并退出块态。
				emit.WriteString(markupOpen)
				emit.WriteString(s)
				m.inBlock = false
				break
			}
			m.hold = s
			break
		}
		inner := s[:k]
		s = s[k+len(markupClose):]
		m.inBlock = false
		m.parent.seen++
		if got, ok := parseMarkupBlock(inner, m.parent.allowed); ok {
			calls = append(calls, got...)
			m.parent.converted += len(got)
			continue
		}
		// 块内不是纯调用（夹带正文 / 工具名不合规 / 参数畸形）：原文回吐，绝不吞字节。
		m.parent.rejected++
		emit.WriteString(markupOpen)
		emit.WriteString(inner)
		emit.WriteString(markupClose)
	}
	return emit.String(), calls
}

// Flush 在 choice 不再接收正文（收尾帧已到 / 流结束 / 结构化调用让位）时调用：
// 把仍未判定的尾部缓冲原文交还。未闭合的块连同起始标记一起回吐——上游发了
// 什么就透出什么。调用后状态机清空，后续 Feed 视为全新输入。
func (m *markupMachine) Flush() string {
	if m == nil {
		return ""
	}
	out := m.hold
	if m.inBlock {
		out = markupOpen + m.hold
	}
	m.hold = ""
	m.inBlock = false
	return out
}

// markupStartCandidate 返回 s 中最早的「可能是 markupOpen 起点」的下标：该位置之后
// 要么正好是完整的 markupOpen，要么是 markupOpen 的一个前缀（被增量边界截断）。
// 没有候选返回 -1。
func markupStartCandidate(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] != '<' {
			continue
		}
		rest := s[i:]
		if len(rest) >= len(markupOpen) {
			if rest[:len(markupOpen)] == markupOpen {
				return i
			}
			continue
		}
		if strings.HasPrefix(markupOpen, rest) {
			return i
		}
	}
	return -1
}

// parseMarkupBlock 解析 calls 块内部（外层起止标记之间的内容）。
//
// 成功要求：块内除 invoke 与空白外没有别的字节；每个 invoke 都闭合；每个 invoke 的
// 参数列表合法；工具名判定通过。工具名判定按 allowed 是否为空分两级：
// 名单非空走严格比对，名单为空走形态检查。任一不满足返回 ok=false，由调用方原文回吐。
func parseMarkupBlock(inner string, allowed map[string]bool) ([]MarkupCall, bool) {
	strict := len(allowed) > 0
	var out []MarkupCall
	pos := 0
	for {
		loc := markupInvokeRe.FindStringSubmatchIndex(inner[pos:])
		if loc == nil {
			break
		}
		if strings.TrimSpace(inner[pos:pos+loc[0]]) != "" {
			return nil, false // invoke 之前夹带了正文
		}
		name := inner[pos+loc[2] : pos+loc[3]]
		bodyStart := pos + loc[1]
		end := strings.Index(inner[bodyStart:], markupInvokeClose)
		if end < 0 {
			return nil, false // invoke 未闭合
		}
		body := inner[bodyStart : bodyStart+end]
		pos = bodyStart + end + len(markupInvokeClose)
		args, ok := parseMarkupParams(body)
		if !ok {
			return nil, false
		}
		if strict {
			if !allowed[name] {
				return nil, false // 工具名不在客户端声明的 tools 里 → 不认，原文透出
			}
		} else if !markupToolNameRe.MatchString(name) {
			return nil, false // 名单缺失时的弱判定：名字不像工具名 → 不认，原文透出
		}
		out = append(out, MarkupCall{Name: name, Arguments: args})
	}
	if len(out) == 0 {
		return nil, false
	}
	if strings.TrimSpace(inner[pos:]) != "" {
		return nil, false // 最后一个 invoke 之后夹带了正文
	}
	return out, true
}

// parseMarkupParams 解析一个 invoke 内部的 parameter 列表，产出 JSON 对象文本。
//
// string="true" 的取值按原文字符串处理（走 JSON 转义，命令里的换行/引号/反斜杠
// 原样保留）；其余先按 JSON 解析，失败则退化为字符串——参数值永远不丢，最坏情况
// 是类型不如模型本意。无参数的工具产出 "{}"。
func parseMarkupParams(body string) (string, bool) {
	args := map[string]any{}
	pos := 0
	for {
		loc := markupParamRe.FindStringSubmatchIndex(body[pos:])
		if loc == nil {
			break
		}
		if strings.TrimSpace(body[pos:pos+loc[0]]) != "" {
			return "", false
		}
		name := body[pos+loc[2] : pos+loc[3]]
		isString := loc[4] >= 0 && body[pos+loc[4]:pos+loc[5]] == "true"
		valStart := pos + loc[1]
		end := strings.Index(body[valStart:], markupParamClose)
		if end < 0 {
			return "", false
		}
		raw := body[valStart : valStart+end]
		pos = valStart + end + len(markupParamClose)
		if isString {
			args[name] = raw
			continue
		}
		var v any
		if json.Unmarshal([]byte(strings.TrimSpace(raw)), &v) == nil {
			args[name] = v
		} else {
			args[name] = raw
		}
	}
	if strings.TrimSpace(body[pos:]) != "" {
		return "", false
	}
	out, err := json.Marshal(args)
	if err != nil {
		return "", false
	}
	return string(out), true
}

// ToolNameAllowlist 从客户端请求体（原始字节）里收集「可用于判定工具调用」的工具名
// 集合，语义见 ToolNameAllowlistDoc。解析失败按空处理（不编造名单）。
func ToolNameAllowlist(body []byte) map[string]bool {
	var req map[string]any
	if json.Unmarshal(body, &req) != nil {
		return nil
	}
	return ToolNameAllowlistDoc(req)
}

// ToolNameAllowlistDoc 从 Chat 形态请求文档里收集「可用于判定工具调用」的工具名集合。
//
// 两个来源都要，因为二者会分别缺失：
//  1. 本请求声明的工具：OpenAI tools[].function.name、Responses 风格裸 tools[].name、
//     旧版 functions[].name。tools 声明在中转环节丢失是实测常态。
//  2. 会话历史里出现过的工具：messages[].tool_calls[].function.name 与旧版
//     messages[].function_call.name。多轮会话里这份名单几乎总在，且天然可信——
//     它是客户端自己写进历史的工具名。
//
// 两个来源都为空 → 返回 nil，由调用方决定是否启用弱判定（见 NewMarkupRepair）。
func ToolNameAllowlistDoc(obj map[string]any) map[string]bool {
	if obj == nil {
		return nil
	}
	names := map[string]bool{}
	if list, ok := obj["tools"].([]any); ok {
		for _, v := range list {
			t, ok := v.(map[string]any)
			if !ok {
				continue
			}
			if fn, ok := t["function"].(map[string]any); ok {
				if s, ok := fn["name"].(string); ok && s != "" {
					names[s] = true
					continue
				}
			}
			if s, ok := t["name"].(string); ok && s != "" {
				names[s] = true
			}
		}
	}
	if list, ok := obj["functions"].([]any); ok {
		for _, v := range list {
			f, ok := v.(map[string]any)
			if !ok {
				continue
			}
			if s, ok := f["name"].(string); ok && s != "" {
				names[s] = true
			}
		}
	}
	if msgs, ok := obj["messages"].([]any); ok {
		for _, v := range msgs {
			m, ok := v.(map[string]any)
			if !ok {
				continue
			}
			if tcs, ok := m["tool_calls"].([]any); ok {
				for _, tv := range tcs {
					tc, ok := tv.(map[string]any)
					if !ok {
						continue
					}
					if fn, ok := tc["function"].(map[string]any); ok {
						if s, ok := fn["name"].(string); ok && s != "" {
							names[s] = true
						}
					}
				}
			}
			if fc, ok := m["function_call"].(map[string]any); ok {
				if s, ok := fc["name"].(string); ok && s != "" {
					names[s] = true
				}
			}
		}
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

// markupStream 在一次响应流上协调各 choice 的标记还原（consume 逐帧调用）。
// 流式与非流式共用同一入口：非流式只是「emit 为 nil 的同一条帧序列」，聚合结果
// 与逐帧透出天然一致。多 choice 各自独立还原（各有状态机与合成帧编号），互不串流。
//
// 非并发安全、不可跨流复用：一条响应流一个对象（由 WithMarkupRepair 在 consume
// 入口构造）。expand 返回的切片由 buf 复用，只能在下一次 expand 之前使用。
type markupStream struct {
	repair  *MarkupRepair
	choices map[int]*markupChoice
	last    map[string]any   // 最近一帧（合成帧的顶字段模板：id/model/created）
	buf     []map[string]any // expand 结果复用缓冲（避免每帧一次切片分配）
}

// markupChoice 是单个 choice 的还原簿记。
type markupChoice struct {
	machine       *markupMachine
	upstreamCalls bool // 出现过结构化 tool_calls：还原层让位（真实调用一律优先）
	maxToolIdx    int  // 上游已用过的最大 tool_call index（合成帧从它之后接续编号）
	restored      int  // 本 choice 已还原的调用数（stop→tool_calls 改写与观测共用）
	finished      bool // 收尾帧已处理（tailFrames 跳过）
}

// newMarkupStream 按还原器构造流协调器；未启用时返回 nil（consume 零开销透传）。
func newMarkupStream(r *MarkupRepair) *markupStream {
	if !r.Enabled() {
		return nil
	}
	return &markupStream{repair: r, choices: map[int]*markupChoice{}}
}

func (ms *markupStream) choice(idx int) *markupChoice {
	st, ok := ms.choices[idx]
	if !ok {
		st = &markupChoice{maxToolIdx: -1}
		ms.choices[idx] = st
	}
	return st
}

// expand 在一帧入站后、进聚合状态之前调用：把「正文中带标记」的帧展开成
// 「正文帧 + N 个调用帧 + 收尾帧」。没有命中标记时原样返回单元素（零改写）。
//
// 展开结果全部经 s.add 走同一条校验管线（index 唯一、finish 顺序、tool_calls
// 完整性校验），合成帧不绕过任何既有约束；收尾帧挪到调用帧之后，避免客户端在
// 读到调用之前就认为回合已结束。
func (ms *markupStream) expand(obj map[string]any) []map[string]any {
	if ms == nil {
		return []map[string]any{obj}
	}
	chs, ok := obj["choices"].([]any)
	if !ok || len(chs) == 0 {
		// 无 choice 帧（usage-only 等）：同样复用缓冲，不更新 last。
		ms.buf = append(ms.buf[:0], obj)
		return ms.buf
	}
	ms.last = obj
	// expand 结果复用 ms.buf：本函数返回后到下一次 expand 之前有效（调用方只在
	// 本帧内按序遍历，不持有）。无可展开内容时仍走单元素复用缓冲，避免热路径分配。
	ms.buf = append(ms.buf[:0], obj)
	type piece struct {
		idx     int
		ch      map[string]any
		m       map[string]any // 正文所在层：delta（增量帧）或 message（整包快照帧）
		text    string
		hasText bool
		finish  string
		hasEnd  bool
		emit    string
		calls   []MarkupCall
	}
	// 绝大多数请求只有一个 choice：小数组走栈，多 choice 才堆分配。
	var pieceBuf [4]piece
	pieces := pieceBuf[:0]
	if len(chs) > len(pieceBuf) {
		pieces = make([]piece, 0, len(chs))
	}
	anyWork := false
	// 第一遍：簿记。结构化 tool_calls 出现 → 本 choice 让位，此前缓冲的未判定
	// 字节原文回吐（先于本帧正文透出）。
	for _, ci := range chs {
		ch, ok := ci.(map[string]any)
		if !ok {
			continue
		}
		p := piece{ch: ch, idx: -1}
		if n, ok := jsondoc.Int(ch["index"]); ok {
			p.idx = int(n)
		}
		st := ms.choice(p.idx)
		if d, ok := ch["delta"].(map[string]any); ok {
			p.m = d
		} else if m, ok := ch["message"].(map[string]any); ok {
			// 整包快照：consume 在 add 之后才把 message 改名成 delta，这里按原名取。
			p.m = m
		}
		if p.m != nil {
			if tcs, ok := p.m["tool_calls"].([]any); ok && len(tcs) > 0 {
				st.upstreamCalls = true
				for _, tci := range tcs {
					if tc, ok := tci.(map[string]any); ok {
						if n, ok := jsondoc.Int(tc["index"]); ok && int(n) > st.maxToolIdx {
							st.maxToolIdx = int(n)
						}
					}
				}
				if st.machine != nil {
					// 让位：此前缓冲的未判定字节原文回吐（先于本帧正文）；
					// 无论有无残留都丢弃状态机，避免空机器让后续每帧都走展开慢路。
					if rest := st.machine.Flush(); rest != "" {
						p.emit += rest
						anyWork = true
					}
					st.machine = nil
				}
			}
			if !st.upstreamCalls {
				if s, ok := p.m["content"].(string); ok && s != "" {
					p.text, p.hasText = s, true
				}
			}
		}
		if v, ok := ch["finish_reason"]; ok && v != nil {
			if s, ok := v.(string); ok {
				p.finish, p.hasEnd = s, true
			}
		}
		if p.hasText || p.hasEnd || st.machine != nil {
			anyWork = true
		}
		pieces = append(pieces, p)
	}
	if !anyWork {
		return ms.buf
	}
	// 第二遍：收尾帧到来先排空机器（残留字节必须先于收尾透出），再喂本帧正文。
	anyCalls := false
	for i := range pieces {
		p := &pieces[i]
		st := ms.choice(p.idx)
		if p.hasEnd {
			if rest := st.machine.Flush(); rest != "" {
				p.emit += rest
			}
			st.machine = nil
			st.finished = true
		}
		if p.hasText {
			if st.machine == nil {
				st.machine = newMarkupMachine(ms.repair)
			}
			emit, calls := st.machine.Feed(p.text)
			p.emit += emit
			p.calls = calls
			if len(calls) > 0 {
				anyCalls = true
				st.restored += len(calls)
			}
		}
	}
	// 第三遍：正文字段落位；命中调用时把 finish 从原帧摘下，挪到调用帧之后重发。
	// 未命中但本 choice 此前还原过调用、上游收尾是 stop → 原地改写为 tool_calls；
	// 本来就没有 finish 的中间帧不动（绝不凭空造收尾），length / content_filter
	// 等其他收尾语义更需要被客户端看到，也不动。
	frames := ms.buf
	var takenFinish map[int]string
	for i := range pieces {
		p := &pieces[i]
		st := ms.choice(p.idx)
		if p.m != nil {
			if p.emit != "" {
				p.m["content"] = p.emit
			} else if p.hasText {
				delete(p.m, "content")
			}
		}
		if anyCalls && p.hasEnd {
			delete(p.ch, "finish_reason")
			if takenFinish == nil {
				takenFinish = map[int]string{}
			}
			takenFinish[p.idx] = p.finish
		} else if p.hasEnd && p.finish == "stop" && st.restored > 0 {
			p.ch["finish_reason"] = "tool_calls"
		}
	}
	for i := range pieces {
		p := &pieces[i]
		st := ms.choice(p.idx)
		for j, call := range p.calls {
			frames = append(frames, ms.callFrame(p.idx, st.maxToolIdx+1+j, call))
		}
	}
	var lastFinish map[string]any
	for i := range pieces {
		p := &pieces[i]
		if fr, ok := takenFinish[p.idx]; ok {
			if fr == "stop" && ms.choice(p.idx).restored > 0 {
				fr = "tool_calls"
			}
			lastFinish = ms.finishFrame(p.idx, fr)
			frames = append(frames, lastFinish)
		}
	}
	// usage 帧位：上游惯例是 usage 挂在收尾帧。拆帧后原帧（正文帧）仍带着它，
	// 而「只读 [DONE] 前最后一帧」的客户端会丢。搬到本帧拆出的最后一个收尾帧上
	//（聚合路径无感：s.add 无论哪帧都并入）；无收尾帧（finish 在后续帧）时不动。
	if lastFinish != nil {
		if u, ok := obj["usage"]; ok {
			delete(obj, "usage")
			lastFinish["usage"] = u
		}
	}
	return frames
}

// tailFrames 在流末尾（[DONE] / EOF）调用：choice 未收尾时，把还原状态机里仍未
// 判定的尾部缓冲原文交还（绝不吞字节）。已收尾的 choice 机器在收尾帧前排空过，
// 这里自然跳过。返回空 = 无需补帧。
func (ms *markupStream) tailFrames() []map[string]any {
	if ms == nil {
		return nil
	}
	idxs := make([]int, 0, len(ms.choices))
	for idx := range ms.choices {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)
	var out []map[string]any
	for _, idx := range idxs {
		st := ms.choices[idx]
		if st.finished || st.machine == nil {
			continue
		}
		if tail := st.machine.Flush(); tail != "" {
			out = append(out, ms.contentFrame(idx, tail))
		}
	}
	return out
}

// markupFrame 按最近一帧的顶字段（id/model/created）构造一个合成 chunk 骨架。
// 只读不拷：模板帧在本帧内不被改动（合成帧构造先于 emit 阶段的字段搬移），
// 省掉每帧一次 map 分配。
func (ms *markupStream) markupFrame() map[string]any {
	frame := map[string]any{"object": "chat.completion.chunk"}
	for _, k := range []string{"id", "model", "created"} {
		if v, ok := ms.last[k]; ok && v != nil {
			frame[k] = v
		}
	}
	return frame
}

// contentFrame 构造一帧只带正文的 chunk（流尾兜底回吐用）。
func (ms *markupStream) contentFrame(idx int, text string) map[string]any {
	frame := ms.markupFrame()
	frame["choices"] = []any{map[string]any{
		"index": float64(idx),
		"delta": map[string]any{"content": text},
	}}
	return frame
}

// callFrame 以本流最近帧为模板，构造一帧只带单个 delta.tool_calls 的 chunk。
// index 一律 float64：帧内数值经解码就是这个类型，合成帧保持同口径，避免任何
// 按类型断言的下游读取静默失败。
func (ms *markupStream) callFrame(idx, base int, call MarkupCall) map[string]any {
	frame := ms.markupFrame()
	frame["choices"] = []any{map[string]any{
		"index": float64(idx),
		"delta": map[string]any{"tool_calls": []any{map[string]any{
			"index":    float64(base),
			"id":       markupCallID(base),
			"type":     "function",
			"function": map[string]any{"name": call.Name, "arguments": call.Arguments},
		}}},
	}}
	return frame
}

// finishFrame 构造一帧只带 finish_reason 的收尾 chunk（空 delta）。
func (ms *markupStream) finishFrame(idx int, finish string) map[string]any {
	frame := ms.markupFrame()
	frame["choices"] = []any{map[string]any{
		"index":         float64(idx),
		"delta":         map[string]any{},
		"finish_reason": finish,
	}}
	return frame
}

// markupCallID 生成一个形态接近 OpenAI 的 tool_call id。序号参与其中，保证同一条
// 响应里多个调用的 id 互不相同。
func markupCallID(idx int) string {
	return fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), idx)
}

// WithMarkupRepair 注册「模型原生工具调用标记」还原器（见 dsml.go）。
//
// 上游在请求未声明 tools 时会把工具调用吐成标记文本，Codex 这类只执行结构化
// function_call 的客户端于是把它当普通正文落进会话历史，回合直接结束、工具一次
// 都没跑。挂上还原器后，这类标记在响应出站前被还原成 tool_calls——流式与非流式
// 共用同一条帧管线（consume 内展开），聚合结果与逐帧透出保持一致。
//
// 传 nil、或传入未启用的还原器 → 与不挂本选项的行为逐字节一致，零分配零改写。
// 命中是 §15 式显式可见的近似变换：调用方必须把还原计数写进请求归档的改写列并
// 打一行日志（见 server/handler.go）。
func WithMarkupRepair(r *MarkupRepair) StreamOption {
	return func(o *streamOptions) { o.markup = newMarkupStream(r) }
}
