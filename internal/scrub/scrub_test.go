package scrub

import (
	"encoding/json"
	"strings"
	"testing"
)

// 与 §6.7.6b 清单逐条对应的夹具（逐字取自历史实现与实测记录）。
const (
	ccIdentity = "You are Claude Code, Anthropic's official CLI for Claude."
	ccBranch   = "Main branch (you will usually use this for PRs)"
	ccHeader   = "x-anthropic-billing-header: cc_version=1.0; cc_entrypoint=cli;"
	codexInstr = "You are a coding agent running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open source project led by OpenAI. You are expected to be precise, safe, and helpful."
	ccFeedback = "To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues"
)

// TestFingerprintsNeutralized 7 类指纹逐条必须被破坏（原文不再出现）。
func TestFingerprintsNeutralized(t *testing.T) {
	cases := []struct {
		name string
		in   string
		gone string // 出站不得再出现的串
		want string // 期望出现（语义保留的证据）
	}{
		{"identity-cli", ccIdentity, ccIdentity, "official CLI tool for Claude"},
		// 桌面版形态：同一句以逗号接后缀（历史漏网点）。
		{"identity-desktop", "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK.", ccIdentity, "official CLI tool for Claude"},
		{"branch", ccBranch, ccBranch, "Default branch (you will usually use this for PRs)"},
		{"header-kv", ccHeader, "x-anthropic-billing-header", ""},
		{"header-bare", "see `x-anthropic-billing-header` for details", "x-anthropic-billing-header", "x-anthropic-billing-hdr"},
		{"header-title-case", "X-Anthropic-Billing-Header: cc_version=9;", "Billing-Header", ""},
		{"cc-kv", "...; cc_version=2.0; cc_entrypoint=cli;", "cc_version", ""},
		{"codex", codexInstr, "running in the Codex CLI,", "running in the Codex CLI tool,"},
		{"codex-identity-gpt5", "You are Codex, a coding agent based on GPT-5.", "You are Codex,", "You are the Codex assistant,"},
		{"codex-identity-openai", "You are Codex, an OpenAI general-purpose agentic assistant.", "You are Codex,", "You are the Codex assistant,"},
		{"feedback", ccFeedback, ccFeedback, "To provide feedback"},
		{"error-code", "upstream returned code=11128 for this request", "11128", "11-128"},
		{"error-code-bare", "错误码 11128", "11128", "11-128"},
		{"error-code-upper", "Code=11128", "11128", "11-128"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := Text(tc.in)
			if strings.Contains(out, tc.gone) {
				t.Errorf("指纹未破坏: %q -> %q", tc.in, out)
			}
			if tc.want != "" && !strings.Contains(out, tc.want) {
				t.Errorf("语义未保留，期望含 %q: %q", tc.want, out)
			}
		})
	}
}

// TestCleanInputUnchanged 无指纹文本逐字不变（含首尾空白——历史实现在这里
// 做全局 TrimSpace，会把用户正文的首尾空白一并改掉）。
func TestCleanInputUnchanged(t *testing.T) {
	cases := []string{
		"ordinary user message",
		"  \n\t leading and trailing whitespace must survive \n\t  ",
		"please use the main branch for this repo",                       // 非精确变体不该动
		"...official CLI for Claude!",                                    // 变体（感叹号）不该动
		"see https://github.com/anthropics/anthropic-cookbook for docs",  // 普通链接不该动
		"You are a coding agent running in a CLI, a terminal-based one.", // 已破坏的 Codex 变体
		"port 1111 and 1112 and 11148 and 11101",                         // 相邻数字不误伤
		"",
	}
	for _, in := range cases {
		if out := Text(in); out != in {
			t.Errorf("干净文本被改写: %q -> %q", in, out)
		}
	}
}

// TestSentinelIsSuperset 哨兵必须是超集：Text 能改动的输入，Sentinel 必须报 true。
// 这条守护"请求体级预检"的正确性——预检漏判会让整层对某些请求静默失效。
func TestSentinelIsSuperset(t *testing.T) {
	for _, in := range []string{ccIdentity, ccBranch, ccHeader, "x-anthropic-billing-header",
		codexInstr, "You are Codex, a coding agent based on GPT-5.", ccFeedback, "code=11128", "cc_version=1.0;", "X-Anthropic-Billing-Header"} {
		body, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"content": in}}})
		if Text(in) != in && !Sentinel(body) {
			t.Errorf("Text 改动了但 Sentinel 漏判: %q", in)
		}
		if Text(in) != in && !defaultLayer.hasSentinel(in) {
			t.Errorf("hasSentinel 漏判: %q", in)
		}
	}
	// 干净请求体必须不被哨兵误报（否则预检形同虚设）。
	clean, _ := json.Marshal(map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "把 main 分支合一下"},
		map[string]any{"role": "assistant", "content": "好，先跑测试。"},
	}})
	if Sentinel(clean) {
		t.Error("干净请求体被哨兵误报")
	}
}

// TestMultipleOccurrences 同一段里多处指纹必须全部处理。
func TestMultipleOccurrences(t *testing.T) {
	in := "code=11128 then 11128 again; cc_a=1; cc_b=2; x-anthropic-billing-header: v=3;"
	out := Text(in)
	if strings.Contains(out, "11128") || strings.Contains(out, "cc_") || strings.Contains(out, "billing-header") {
		t.Errorf("残留指纹: %q", out)
	}
	if strings.Count(out, "11-128") != 2 {
		t.Errorf("两处 11128 应各改写一次: %q", out)
	}
}

// TestCCWordBoundary cc_ 只在词边界触发（对齐历史正则的 \b）。
func TestCCWordBoundary(t *testing.T) {
	// 前面是词字符 → 不是键值段，不动。
	if in := "abc_cc_x=1;"; Text(in) != in {
		// xcc_ 不含 cc_ 前缀的独立词界，但本串含 "cc_" —— 词界判定应阻止删除。
		t.Errorf("非词界的 cc_ 不应被删除: %q", Text(in))
	}
	// 前面是空白/分号 → 正常删除。
	if out := Text(" cc_x=1;"); strings.Contains(out, "cc_x") {
		t.Errorf("词界的 cc_ 应被删除: %q", out)
	}
	// 没有 '=' 的裸 cc_ 前缀不是键值段，保留（避免误删普通文本）。
	if in := "the cc_ prefix"; Text(in) != in {
		t.Errorf("无 '=' 的 cc_ 不应被删除: %q", Text(in))
	}
}

// TestObjectChannels 全部实测通道都要覆盖：content（含多模态 text part）、
// reasoning_content、reasoning、tool_calls[].function.arguments。
func TestObjectChannels(t *testing.T) {
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "无指纹的客户端 system"},
		// 工具调用轮：content 为 null（历史盲区），指纹只在 arguments 里。
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"function": map[string]any{"arguments": `{"command":"echo 11128"}`}},
		}},
		map[string]any{"role": "assistant", "reasoning_content": "upstream said 11128",
			"reasoning": "cc_version=1.0;"},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": ccIdentity},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:x;base64,11128"}},
		}},
	}}
	if !Object(obj) {
		t.Fatal("Object 未报告改动（tool_calls 盲区回归？）")
	}
	raw, _ := json.Marshal(obj)
	out := string(raw)
	// 「无残留」断言必须排除 image part：它故意保留含 11128 的 base64。
	withoutImage := strings.Replace(out, "data:x;base64,11128", "", 1)
	for _, bad := range []string{"11128", "cc_version", "Anthropic's official CLI for Claude"} {
		if strings.Contains(withoutImage, bad) {
			t.Errorf("残留 %q: %s", bad, out)
		}
	}
	// image part 的数据必须原样保留（改写 base64 会破坏图片）。
	if !strings.Contains(out, "data:x;base64,11128") {
		t.Errorf("image part 被误改: %s", out)
	}
	// 干净字段逐字不变。
	if !strings.Contains(out, "无指纹的客户端 system") {
		t.Errorf("干净内容被改动: %s", out)
	}
}

// TestObjectCleanNoChange 干净文档不报告改动（保证调用方零成本跳过）。
func TestObjectCleanNoChange(t *testing.T) {
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": "hi"},
	}}
	if Object(obj) {
		t.Error("干净文档不应报告改动")
	}
	// 无 messages / 非数组形态不得 panic。
	Object(map[string]any{})
	Object(map[string]any{"messages": "not-an-array"})
	Object(map[string]any{"messages": []any{"not-a-map", 42, nil}})
}

// TestNoAllocOnCleanPath 干净路径必须零分配（这是本层能在热路径常开的前提）。
func TestNoAllocOnCleanPath(t *testing.T) {
	clean := strings.Repeat("You are a helpful coding assistant. Follow the user's instructions.\n", 25)
	if n := testing.AllocsPerRun(200, func() { _ = Text(clean) }); n != 0 {
		t.Errorf("干净文本 Text 分配 %v 次，期望 0", n)
	}
	body, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"content": clean}}})
	if n := testing.AllocsPerRun(200, func() { _ = Sentinel(body) }); n != 0 {
		t.Errorf("Sentinel 分配 %v 次，期望 0", n)
	}
}

// TestSentinelCheapOnLargeBody 大请求体上哨兵扫描必须远快于一次完整遍历。
func BenchmarkSentinelLargeBody(b *testing.B) {
	payload := strings.Repeat("这是一个较长的用户消息，用于模拟真实长上下文请求体。", 4000) // ~100KB
	body, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"content": payload}}})
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if Sentinel(body) {
			b.Fatal("不应误报")
		}
	}
}

func BenchmarkTextClean2K(b *testing.B) {
	clean := strings.Repeat("You are a helpful coding assistant. Follow the user's instructions.\n", 25)
	b.SetBytes(int64(len(clean)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if Text(clean) != clean {
			b.Fatal("mutated")
		}
	}
}

func BenchmarkTextHit(b *testing.B) {
	in := strings.Repeat("You are a helpful coding assistant.\n", 25) + "code=11128"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Text(in)
	}
}

// BenchmarkObjectTypical 模拟真实多轮历史（10 条消息），走 Object 全通道。
func BenchmarkObjectTypical(b *testing.B) {
	text := strings.Repeat("You are a helpful coding assistant. Follow the user's instructions.\n", 25)
	obj := map[string]any{"messages": []any{}}
	msgs := obj["messages"].([]any)
	msgs = append(msgs, map[string]any{"role": "system", "content": text})
	for i := 0; i < 4; i++ {
		msgs = append(msgs,
			map[string]any{"role": "user", "content": text},
			map[string]any{"role": "assistant", "content": text, "tool_calls": []any{
				map[string]any{"function": map[string]any{"arguments": `{"path":"README.md"}`}}}})
	}
	obj["messages"] = msgs
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if Object(obj) {
			b.Fatal("干净文档不该改动")
		}
	}
}
