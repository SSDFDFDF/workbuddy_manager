// Package scrub 出站请求体的上游指纹改写（best-effort，可关闭）。
//
// 背景：上游内容审核按**逐字字面**拒绝（不做语义判断，见
// docs/WORKBUDDY_CLIENT_FEATURES_ANALYSIS.md §6.7.6b 的 7 类指纹清单）。
// 其中 system/developer 来源可由 prompt.mode=replace/after 整体规避；
// **user/assistant/tool 来源清不掉**（用户粘帖报错原文、终端回显、
// 工具参数、思维链字段、项目规则文件），只能逐字改写。
//
// # 设计取舍（与历史实现的关键差异）
//
// 历史实现（已删除的 internal/upstream/sanitize.go）在**每条消息的每段文本**上
// 跑「7×strings.Contains + 1 个 (?i) 正则」预检，命中后依次 5×ReplaceAll +
// 3 个正则 + 一个 cc_ 定点循环。实测 2KB 干净文本 24µs、典型 10 条消息请求
// 220µs——**绝大多数请求没有任何指纹，却要全额付这笔钱**。
//
// 本实现按「先证伪、再精确定位」重排：
//
//	A. 请求体级哨兵预检：整包 bytes.Index 扫 5 个必要子串（其中一个大小写变体
//	   双查）。任一不中 → 直接判定「无指纹」，不解析、不遍历、零分配。
//	   哨兵是超集条件：任何指纹命中都必先命中对应哨兵（有测试守护）。
//	B. 命中后单遍改写：5 条字面串交给一个 strings.Replacer（内部 trie，
//	   单次扫描、不重叠、无命中时返回原串）；2 条变长/大小写不敏感的模式
//	   （header 键值段、尾随 cc_ 键值段）由一次手写字节扫描处理，
//	   顺带完成裸键名的缩写——**没有正则**。
//	C. 不做全局 TrimSpace。历史实现在每段"净化过"的文本尾部 TrimSpace，
//	   会把用户正文的首尾空白一并改掉（与指纹无关的语义变更）；本实现
//	   只在被删除的键值段跨度内消费其尾随空白。
//
// 失败方向是**fail-open**：漏判只等于「这一层没生效」，绝不阻断或改写无关内容。
// 因此哨兵只查两个大小写变体（真实客户端不产出 `aNTHROPIC` 这类混合大小写），
// 换来的是 bytes.Index 的向量化扫描速度。
package scrub

import (
	"bytes"
	"strings"
)

// 已知指纹的字面查表（与 §6.7.6b 清单一一对应）。
//
// 身份句的匹配串**不含结尾标点**：CLI 版以 '.' 收尾、桌面版以 ',' 接
// "running within the Claude Agent SDK"，去标点后两种形态一并覆盖
// （历史实现踩过"带句号只匹配 CLI 版、桌面版漏网"的坑）。
var literals = []string{
	// #3 身份句 → 一词替换（official CLI for Claude → …CLI tool for Claude）
	"You are Claude Code, Anthropic's official CLI for Claude",
	"You are Claude Code, Anthropic's official CLI tool for Claude",
	// #4 注入指令句 → 一词替换
	"Main branch (you will usually use this for PRs)",
	"Default branch (you will usually use this for PRs)",
	// #5 Codex 身份句（三句模板的首句）→ 一词替换
	"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
	"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.",
	// #5b Codex 备用身份句：本地核对 Codex v0.153.4 二进制发现多个以
	// "You are Codex," 开头的变体（"...a coding agent based on GPT-5" /
	// "...an OpenAI general-purpose agentic assistant" / "...an agent based on GPT-5"）。
	// 只对共同前缀做一词插入，一条覆盖全部变体；替换串不含匹配串。
	"You are Codex,",
	"You are the Codex assistant,",
	// #6 反馈句（需整句同现）→ 一词替换
	"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
	"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
	// #7 反探测数字：上游只要请求体里出现裸 11128 就整单拦截，与该数字的上下文
	// 无关（"code=11128" / 裸串 / "错误码 11128" 全部命中；相邻的 11148 / 11101 /
	// 11115 / 99999 均放行）。11128 正是本类拦截自身的错误码，上游据此识别
	// "在讨论/回显其内部错误码"的请求——所以把报错原文粘进对话再问模型，
	// 这个动作本身就会再触发一次。插连字符保留可读性与指代
	// （零宽空格无效，实测上游会归一化）。
	"11128", "11-128",
}

// literalReplacer 上表字面串的单遍改写器（trie，无命中时返回原串不分配）。
var literalReplacer = strings.NewReplacer(literals...)

// literalNotes 与 literals 的 match→replace 对一一对应（面板展示用说明）。
var literalNotes = []string{
	"#3 Claude Code 身份句（official CLI → CLI tool）",
	"#4 PR 分支注入句（Main branch → Default branch）",
	"#5 Codex 身份句（Codex CLI → Codex CLI tool）",
	"#5b Codex 备用身份句（You are Codex, → You are the Codex assistant,）",
	"#6 反馈 issue 句（give → provide feedback）",
	"#7 反探测错误码 11128（上游见到裸串即整单拦截）",
}

// BuiltinRule 一条内置指纹规则的只读描述（面板展示用）。
type BuiltinRule struct {
	Kind    string `json:"kind"`              // literal（逐字替换）/ pattern（变长模式）
	Match   string `json:"match"`             // 匹配串或模式描述
	Replace string `json:"replace,omitempty"` // 替换文本；空串表示整段删除
	Note    string `json:"note,omitempty"`    // 说明
}

// BuiltinRules 返回内置指纹表的结构化描述（供面板模态框展示）。
//
// literal 部分直接从 literals 派生，不会与改写逻辑漂移（有测试守护长度一致）；
// pattern 部分描述 stripKeyValues 处理的两类变长/大小写不敏感模式。
func BuiltinRules() []BuiltinRule {
	out := make([]BuiltinRule, 0, len(literals)/2+3)
	for i := 0; i+1 < len(literals); i += 2 {
		note := ""
		if j := i / 2; j < len(literalNotes) {
			note = literalNotes[j]
		}
		out = append(out, BuiltinRule{
			Kind:    "literal",
			Match:   literals[i],
			Replace: literals[i+1],
			Note:    note,
		})
	}
	out = append(out,
		BuiltinRule{
			Kind:  "pattern",
			Match: headerName + ":<值>[;]",
			Note:  "#1 图灵/计费 header 键值段整段删除（大小写不敏感）",
		},
		BuiltinRule{
			Kind:    "pattern",
			Match:   headerName + "（裸键名）",
			Replace: headerShort,
			Note:    "#1 残留裸键名缩写为 " + headerShort,
		},
		BuiltinRule{
			Kind:  "pattern",
			Match: ccPrefix + "<name>=<值>[;]",
			Note:  "#2 尾随 cc_ 键值段整段删除（词边界 + 大小写不敏感）",
		},
	)
	return out
}

const (
	// headerName 图灵/计费 header 键名（上游只看键名不看值）。
	headerName = "x-anthropic-billing-header"
	// headerShort 残留裸键名的最小缩写（破坏逐字匹配、保留可读性）。
	// 键值形态被整体删除后只剩引用/示例形态的裸键名，实测裸键名同样触发拦截。
	headerShort = "x-anthropic-billing-hdr"
	// ccPrefix 该 header 的值段前缀（尾随裸键值形态）。
	ccPrefix = "cc_"
)

// sentinels 请求体级预检哨兵：任一指纹命中都必先命中其中一个。
//
// 覆盖关系（有测试守护）：
//
//	#1 header 键名 / #3 身份句 / #6 反馈句 → "nthropic"（或全大写变体）
//	#2 cc_ 键值                          → "cc_"
//	#4 Main branch (                     → "Main branch ("
//	#5 Codex CLI                         → "Codex CLI"
//	#5b You are Codex,                    → "You are Codex,"
//	#7 11128                             → "11128"
var sentinels = [][]byte{
	[]byte("nthropic"), []byte("NTHROPIC"), // 大小写不敏感的 "anthropic"
	[]byte(ccPrefix),
	[]byte("Main branch ("),
	[]byte("Codex CLI"),
	[]byte("You are Codex,"),
	[]byte("11128"),
}

// Sentinel 报告请求体是否可能含指纹（超集判定：false 等价于"确定无指纹"）。
//
// 在整包原始字节上做，不做 JSON 解码——代价是每个哨兵一次 bytes.Index，
// 典型请求（无指纹）在这里就返回，后续解析与遍历全部省掉。
//
// 已知边界（fail-open，可接受）：若客户端把哨兵字符写成 JSON 转义形式
// （如 `\u0031` 代替 '1'），本预检会漏判 → 这一层对该请求不生效，
// 与"未启用该层"等价，不会改坏内容。真实客户端不产出此类转义。
func Sentinel(body []byte) bool {
	for _, s := range sentinels {
		if bytes.Index(body, s) >= 0 {
			return true
		}
	}
	return false
}

// Text 改写单段文本。无指纹时返回**同一字符串**（零分配、逐字不变）。
func Text(s string) string {
	if !hasSentinel(s) {
		return s
	}
	// 变长/大小写不敏感的两类先扫（它们要按跨度删除，交给 trie 无法表达）。
	if hasFold(s, headerName) || hasFold(s, ccPrefix) {
		s = stripKeyValues(s)
	}
	// 字面串一遍过（含 11128 → 11-128）。
	return literalReplacer.Replace(s)
}

// hasSentinel 报告字符串是否可能含指纹（与 Sentinel 同一套哨兵，作用于字符串）。
func hasSentinel(s string) bool {
	return strings.Contains(s, "nthropic") || strings.Contains(s, "NTHROPIC") ||
		strings.Contains(s, ccPrefix) ||
		strings.Contains(s, "Main branch (") ||
		strings.Contains(s, "Codex CLI") ||
		strings.Contains(s, "You are Codex,") ||
		strings.Contains(s, "11128")
}

// stripKeyValues 单遍扫描：删除 header 键值段与尾随 cc_ 键值段，
// 并把残留的裸 header 键名缩写为 headerShort。
//
// 语义对齐历史实现的三条正则，但**没有正则**：
//
//	(?i)x-anthropic-billing-header:[^;\n]*;?\s*   键值段整段删除
//	(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*              尾随裸键值循环删除
//	(?i)x-anthropic-billing-header                残留裸键名缩写
//
// 与历史实现的差异：只消费「被删段落的尾随空白」，不 TrimSpace 整段文本
// （历史实现会把用户正文首尾空白一并改掉）。
func stripKeyValues(s string) string {
	// 快速路径：两类模式都不在 → 直接返回原串。
	if !hasFold(s, headerName) && !hasFold(s, ccPrefix) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		// 1) header 键名（大小写不敏感）。
		if n := matchFold(s, i, headerName); n > 0 {
			after := i + n
			// 键值形态：紧跟 ':' → 整段删除（含值、可选 ';'、尾随空白）。
			if after < len(s) && s[after] == ':' {
				i = kvEnd(s, after+1)
				continue
			}
			// 裸键名形态（无冒号）：最小缩写，保留可读性。
			b.WriteString(headerShort)
			i = after
			continue
		}
		// 2) 尾随 cc_ 键值（要求词边界，对齐历史正则的 \b）。
		if s[i] == 'c' || s[i] == 'C' {
			if (i == 0 || !isWordByte(s[i-1])) && matchFold(s, i, ccPrefix) > 0 {
				if j, ok := ccKVEnd(s, i); ok {
					i = j
					continue
				}
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// kvEnd 从 header 的 ':' 之后开始，返回键值段结束位置：
// 值取到 ';' 或换行为止，随后吞掉一个可选的 ';' 与其后的空白。
// 对齐历史正则的 `[^;\n]*;?\s*`。
func kvEnd(s string, i int) int {
	for i < len(s) && s[i] != ';' && s[i] != '\n' {
		i++
	}
	if i < len(s) && s[i] == ';' {
		i++
	}
	return skipSpace(s, i)
}

// ccKVEnd 尝试把 s[i:] 解析为 `cc_<name>=<value>[;]<空白>`：
// 成功返回键值段结束位置与 true；不是合法键值形态时返回 false（调用方逐字输出）。
// name 只接受 [A-Za-z0-9_] 且至少一字符，其后必须是 '='（对齐 `[a-z0-9_]+=`）。
func ccKVEnd(s string, i int) (int, bool) {
	j := i + len(ccPrefix)
	start := j
	for j < len(s) && isWordByte(s[j]) {
		j++
	}
	if j == start || j >= len(s) || s[j] != '=' {
		return 0, false
	}
	return kvEnd(s, j+1), true
}

// skipSpace 跳过连续空白（对齐正则 `\s*`；Go 正则的 \s 含 \t\n\f\r 与空格）。
func skipSpace(s string, i int) int {
	for i < len(s) {
		switch s[i] {
		case ' ', '\t', '\n', '\v', '\f', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// isWordByte 报告 b 是否属于 [A-Za-z0-9_]（历史正则 \b 与 [a-z0-9_] 的口径）。
func isWordByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// hasFold 报告 s 是否含 sub 的大小写不敏感形态（ASCII，无分配）。
func hasFold(s, sub string) bool { return indexFold(s, sub) >= 0 }

// indexFold 大小写不敏感（ASCII）子串查找，无分配。
func indexFold(s, sub string) int {
	n, m := len(s), len(sub)
	if m == 0 || n < m {
		return -1
	}
	c0 := lowerASCII(sub[0])
	for i := 0; i+m <= n; i++ {
		if lowerASCII(s[i]) != c0 {
			continue
		}
		j := 1
		for j < m && lowerASCII(s[i+j]) == lowerASCII(sub[j]) {
			j++
		}
		if j == m {
			return i
		}
	}
	return -1
}

// matchFold 报告 s[i:] 是否以 sub 开头（大小写不敏感，ASCII）；
// 命中返回 sub 的长度，未命中返回 -1。
func matchFold(s string, i int, sub string) int {
	if i+len(sub) > len(s) {
		return -1
	}
	for k := 0; k < len(sub); k++ {
		if lowerASCII(s[i+k]) != lowerASCII(sub[k]) {
			return -1
		}
	}
	return len(sub)
}

// lowerASCII ASCII 小写化（非字母原样返回）。
func lowerASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

// Object 就地改写请求体文档中的指纹承载字段，返回是否有改动。
//
// 覆盖全部实测通道（历史实现的盲区修复保持不变）：
//   - messages[].content：字符串形态与多模态数组的 text part（image part 不动）；
//   - messages[].reasoning_content / reasoning：思维链字段（system 指纹会被
//     镜像进推理文本，裸 11128 这类反探测串同样致命）；
//   - messages[].tool_calls[].function.arguments：字符串化 JSON，按文本处理。
//     **这块曾是盲区**：工具调用轮的 content 常为 null，历史实现遇到 content
//     缺失就 continue，整条消息连同 tool_calls 一起被跳过。
//
// 逐字段调用 Text：无指纹的字段返回原串，因此"整包命中但只有一处指纹"时
// 只有那一个字段被改写。
func Object(obj map[string]any) bool {
	msgs, _ := obj["messages"].([]any)
	if len(msgs) == 0 {
		return false
	}
	changed := false
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if c, ok := m["content"]; ok {
			// 按类型就地分支，不走 `content(any) (any, bool)` 那种签名：
			// 把 string 当 any 返回会引发一次装箱分配，而干净路径要求 0 分配。
			switch v := c.(type) {
			case string:
				if n := Text(v); n != v {
					m["content"] = n
					changed = true
				}
			case []any:
				if textParts(v) {
					changed = true
				}
			}
		}
		for _, k := range [2]string{"reasoning_content", "reasoning"} {
			if s, ok := m[k].(string); ok {
				if ns := Text(s); ns != s {
					m[k] = ns
					changed = true
				}
			}
		}
		if tc, ok := m["tool_calls"].([]any); ok && toolCalls(tc) {
			changed = true
		}
	}
	return changed
}

// textParts 改写入多模态 content 数组中的 text part（image / audio part 不动：
// 其 base64 里不会有指纹，且改写会破坏数据）。
func textParts(parts []any) bool {
	changed := false
	for _, p := range parts {
		part, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if t, ok := part["text"].(string); ok {
			if nt := Text(t); nt != t {
				part["text"] = nt
				changed = true
			}
		}
	}
	return changed
}

// toolCalls 改写入参（字符串化 JSON），只动 arguments 文本。
func toolCalls(list []any) bool {
	changed := false
	for _, raw := range list {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := call["function"].(map[string]any)
		if !ok {
			continue
		}
		args, ok := fn["arguments"].(string)
		if !ok {
			continue
		}
		if na := Text(args); na != args {
			fn["arguments"] = na
			changed = true
		}
	}
	return changed
}
