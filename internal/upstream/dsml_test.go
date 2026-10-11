package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// leakedFixture 是生产环境真实泄漏正文（正文里躺着完整的原生标记块），
// 取自上游回归夹具 internal/upstream/testdata/leaked_markup.txt。
func leakedFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/leaked_markup.txt")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(b)
}

func chunk(t *testing.T, delta map[string]any, finish any) string {
	t.Helper()
	ch := map[string]any{"index": 0, "delta": delta}
	if finish != nil {
		ch["finish_reason"] = finish
	}
	b, err := json.Marshal(map[string]any{"id": "x", "model": "m", "choices": []any{ch}})
	if err != nil {
		t.Fatal(err)
	}
	return "data: " + string(b) + "\n\n"
}

// TestMarkupMachineFixture：真实泄漏正文 → 两条 exec_command 调用；正文前缀保留。
func TestMarkupMachineFixture(t *testing.T) {
	fix := leakedFixture(t)
	m := newMarkupMachine(NewMarkupRepair(nil, true))
	emit, calls := m.Feed(fix)
	emit += m.Flush()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	if calls[0].Name != "exec_command" || calls[1].Name != "exec_command" {
		t.Fatalf("names = %q %q", calls[0].Name, calls[1].Name)
	}
	if !strings.HasPrefix(emit, "我先看一下") {
		t.Fatalf("prefix lost: %q", emit[:min(len(emit), 40)])
	}
	if strings.Contains(emit, "DSML") {
		t.Fatalf("markup leaked: %q", emit)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("arguments not JSON: %v", err)
	}
	if !strings.Contains(args["cmd"].(string), "git status") {
		t.Fatalf("cmd mangled: %v", args["cmd"])
	}
}

// TestMarkupMachineStrict：名单非空走严格比对；名单外工具名原文透出。
func TestMarkupMachineStrict(t *testing.T) {
	fix := leakedFixture(t)
	m := newMarkupMachine(NewMarkupRepair(map[string]bool{"other_tool": true}, false))
	emit, calls := m.Feed(fix)
	emit += m.Flush()
	if len(calls) != 0 {
		t.Fatalf("strict should reject undeclared name, got %d calls", len(calls))
	}
	if emit != fix {
		t.Fatalf("strict rejection must be byte-identical")
	}
}

// TestMarkupMachineNoSwallow：拿不准的分支一律原文回吐。
func TestMarkupMachineNoSwallow(t *testing.T) {
	weak := func() *markupMachine { return newMarkupMachine(NewMarkupRepair(nil, true)) }
	// 未闭合块。
	m := weak()
	emit, calls := m.Feed("前缀<" + markupPipe + "DSML" + markupPipe + " calls>永远不闭合")
	emit += m.Flush()
	if len(calls) != 0 || emit != "前缀<"+markupPipe+"DSML"+markupPipe+" calls>永远不闭合" {
		t.Fatalf("unclosed must round-trip verbatim: %q %d", emit, len(calls))
	}
	// 块内夹带正文。
	m = weak()
	block := "<" + markupPipe + "DSML" + markupPipe + " calls>夹带正文<" + markupPipe + "DSML" + markupPipe + " invoke name=\"exec_command\"></" + markupPipe + "DSML" + markupPipe + " invoke></" + markupPipe + "DSML" + markupPipe + " calls>"
	emit, calls = m.Feed(block)
	emit += m.Flush()
	if len(calls) != 0 || emit != block {
		t.Fatalf("inline text must round-trip verbatim")
	}
	// 弱判定：名字不像标识符（含尖括号占位符）。
	m = weak()
	bad := "<" + markupPipe + "DSML" + markupPipe + " calls><" + markupPipe + "DSML" + markupPipe + " invoke name=\"<tool_name>\"></" + markupPipe + "DSML" + markupPipe + " invoke></" + markupPipe + "DSML" + markupPipe + " calls>"
	emit, calls = m.Feed(bad)
	emit += m.Flush()
	if len(calls) != 0 || emit != bad {
		t.Fatalf("placeholder name must round-trip verbatim")
	}
}

// TestMarkupMachineFragmented：标记被增量边界切成任意两片，仍完整还原。
func TestMarkupMachineFragmented(t *testing.T) {
	fix := leakedFixture(t)
	mid := strings.Index(fix, "<"+markupPipe)
	for _, split := range []int{1, mid, mid + 3, len(fix) - 5} {
		if split <= 0 || split >= len(fix) {
			continue
		}
		m := newMarkupMachine(NewMarkupRepair(nil, true))
		e1, c1 := m.Feed(fix[:split])
		e2, c2 := m.Feed(fix[split:])
		if len(c1)+len(c2) != 2 {
			t.Fatalf("split %d: calls = %d", split, len(c1)+len(c2))
		}
		if strings.Contains(e1+e2, "DSML") {
			t.Fatalf("split %d: markup leaked", split)
		}
	}
}

// TestMarkupAggregate：非流式聚合——标记还原成 message.tool_calls，
// finish_reason 改写为 tool_calls，正文前缀保留、标记零残留。
func TestMarkupAggregate(t *testing.T) {
	fix := leakedFixture(t)
	sse := chunk(t, map[string]any{"content": fix}, nil) +
		chunk(t, map[string]any{}, "stop") + "data: [DONE]\n\n"
	r, err := Aggregate(strings.NewReader(sse), WithMarkupRepair(NewMarkupRepair(nil, true)))
	if err != nil {
		t.Fatal(err)
	}
	ch := r["choices"].([]any)[0].(map[string]any)
	msg := ch["message"].(map[string]any)
	tcs, ok := msg["tool_calls"].([]any)
	if !ok || len(tcs) != 2 {
		t.Fatalf("tool_calls = %v", msg["tool_calls"])
	}
	if ch["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v", ch["finish_reason"])
	}
	if !strings.HasPrefix(msg["content"].(string), "我先看一下") || strings.Contains(msg["content"].(string), "DSML") {
		t.Fatalf("content = %q", msg["content"])
	}
	for _, v := range tcs {
		tc := v.(map[string]any)
		fn := tc["function"].(map[string]any)
		if tc["id"] == "" || tc["type"] != "function" || fn["name"] == "" {
			t.Fatalf("incomplete call: %v", tc)
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(fn["arguments"].(string)), &args); err != nil {
			t.Fatalf("arguments not JSON: %v", err)
		}
	}
}

// TestMarkupAggregateLengthFinish：finish 非 stop 时不改写 finish_reason
// （length 语义客户端更需要看到）。
func TestMarkupAggregateLengthFinish(t *testing.T) {
	fix := leakedFixture(t)
	sse := chunk(t, map[string]any{"content": fix}, "length") + "data: [DONE]\n\n"
	r, err := Aggregate(strings.NewReader(sse), WithMarkupRepair(NewMarkupRepair(nil, true)))
	if err != nil {
		t.Fatal(err)
	}
	ch := r["choices"].([]any)[0].(map[string]any)
	if ch["finish_reason"] != "length" {
		t.Fatalf("finish_reason = %v", ch["finish_reason"])
	}
}

// TestMarkupStream：流式——一帧拆成正文帧 + 调用帧 + 收尾帧；finish 在调用之后；
// 输出零标记残留；正文前缀先于调用帧透出。
func TestMarkupStream(t *testing.T) {
	fix := leakedFixture(t)
	sse := chunk(t, map[string]any{"content": fix}, nil) +
		chunk(t, map[string]any{}, "stop") + "data: [DONE]\n\n"
	w := httptest.NewRecorder()
	if err := StreamHint(w, strings.NewReader(sse), nil, WithMarkupRepair(NewMarkupRepair(nil, true))); err != nil {
		t.Fatal(err)
	}
	out := w.Body.String()
	if strings.Contains(out, "DSML") {
		t.Fatalf("markup leaked:\n%s", out)
	}
	var callIdx, finishIdx, textIdx = -1, -1, -1
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var f struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []any  `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &f) != nil || len(f.Choices) == 0 {
			continue
		}
		d := f.Choices[0]
		if len(d.Delta.ToolCalls) > 0 {
			if callIdx < 0 {
				callIdx = strings.Index(out, line)
			}
		}
		if d.FinishReason == "tool_calls" {
			finishIdx = strings.Index(out, line)
		}
		if strings.HasPrefix(d.Delta.Content, "我先看一下") {
			textIdx = strings.Index(out, line)
		}
	}
	if callIdx < 0 || finishIdx < 0 || textIdx < 0 {
		t.Fatalf("missing frames (call=%d finish=%d text=%d):\n%s", callIdx, finishIdx, textIdx, out)
	}
	if !(textIdx < callIdx && callIdx < finishIdx) {
		t.Fatalf("frame order broken: text=%d call=%d finish=%d", textIdx, callIdx, finishIdx)
	}
}

// TestMarkupStreamRealCallsWin：同一流里出现结构化 tool_calls → 还原层让位，
// 此前缓冲的未判定字节原文回吐，不产生合成调用。
func TestMarkupStreamRealCallsWin(t *testing.T) {
	part := "<" + markupPipe + "DSML" + markupPipe // 半个开始标记：会进机器 hold
	sse := chunk(t, map[string]any{"content": "正文" + part}, nil) +
		chunk(t, map[string]any{"tool_calls": []any{map[string]any{
			"index": float64(0), "id": "real", "type": "function",
			"function": map[string]any{"name": "real_tool", "arguments": "{}"},
		}}}, nil) +
		chunk(t, map[string]any{}, "tool_calls") + "data: [DONE]\n\n"
	w := httptest.NewRecorder()
	repair := NewMarkupRepair(nil, true)
	if err := StreamHint(w, strings.NewReader(sse), nil, WithMarkupRepair(repair)); err != nil {
		t.Fatal(err)
	}
	out := w.Body.String()
	if repair.Converted() != 0 {
		t.Fatalf("markup repaired despite real calls: %d", repair.Converted())
	}
	if !strings.Contains(out, "real_tool") {
		t.Fatalf("real call lost:\n%s", out)
	}
	// 让位时 hold 里的半个标记必须原文回吐（绝不吞字节）。
	// （比对去样含 < 的部分：json.Marshal 会把 < 转义成 \u003c，全角竖线不转义。）
	if !strings.Contains(out, markupPipe+"DSML"+markupPipe) {
		t.Fatalf("held bytes swallowed:\n%s", out)
	}
}

// TestMarkupStreamFragmented：标记跨帧碎片化（流式逐 delta），仍完整还原。
// 按 rune 边界切分（字节切分会切断多字节字符，json.Marshal 会替换无效序列，
// 破坏标记字节——真实上游不会发出无效 UTF-8）。
func TestMarkupStreamFragmented(t *testing.T) {
	fix := leakedFixture(t)
	runes := []rune(fix)
	var b strings.Builder
	for i := 0; i < len(runes); i += 9 {
		end := min(i+9, len(runes))
		b.WriteString(chunk(t, map[string]any{"content": string(runes[i:end])}, nil))
	}
	b.WriteString(chunk(t, map[string]any{}, "stop"))
	b.WriteString("data: [DONE]\n\n")
	w := httptest.NewRecorder()
	repair := NewMarkupRepair(nil, true)
	if err := StreamHint(w, strings.NewReader(b.String()), nil, WithMarkupRepair(repair)); err != nil {
		t.Fatal(err)
	}
	if repair.Converted() != 2 {
		t.Fatalf("converted = %d, want 2 (body):\n%s", repair.Converted(), w.Body.String())
	}
	if strings.Contains(w.Body.String(), "DSML") {
		t.Fatalf("markup leaked")
	}
}

// TestMarkupSnapshotPath：整包 JSON 响应（非 SSE）同样还原。
func TestMarkupSnapshotPath(t *testing.T) {
	fix := leakedFixture(t)
	obj := map[string]any{
		"id": "x", "model": "m", "object": "chat.completion",
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": fix},
		}},
	}
	raw, _ := json.Marshal(obj)
	r, err := Aggregate(strings.NewReader(string(raw)), WithMarkupRepair(NewMarkupRepair(nil, true)))
	if err != nil {
		t.Fatal(err)
	}
	ch := r["choices"].([]any)[0].(map[string]any)
	msg := ch["message"].(map[string]any)
	if tcs, ok := msg["tool_calls"].([]any); !ok || len(tcs) != 2 {
		t.Fatalf("tool_calls = %v", msg["tool_calls"])
	}
	if ch["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v", ch["finish_reason"])
	}
}

// TestMarkupDisabledPassthrough：未启用（名单空且弱判定关）→ 与不挂选项逐字节一致。
func TestMarkupDisabledPassthrough(t *testing.T) {
	fix := leakedFixture(t)
	sse := chunk(t, map[string]any{"content": fix}, "stop") + "data: [DONE]\n\n"
	a, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Aggregate(strings.NewReader(sse), WithMarkupRepair(NewMarkupRepair(nil, false)))
	if err != nil {
		t.Fatal(err)
	}
	ab, bb := mustJSON(t, a), mustJSON(t, b)
	if ab != bb {
		t.Fatalf("disabled repair changed bytes")
	}
	if !strings.Contains(ab, "DSML") {
		t.Fatalf("disabled repair must not convert")
	}
}

// TestMarkupMultiChoice：多 choice 各自独立还原，互不串流。
func TestMarkupMultiChoice(t *testing.T) {
	fix := leakedFixture(t)
	mk := func(idx int, content string, finish any) string {
		ch := map[string]any{"index": idx, "delta": map[string]any{"content": content}}
		if finish != nil {
			ch["finish_reason"] = finish
		}
		b, _ := json.Marshal(map[string]any{"id": "x", "model": "m", "choices": []any{ch}})
		return "data: " + string(b) + "\n\n"
	}
	sse := mk(0, fix, nil) + mk(1, "choice1 纯正文", nil) +
		mk(0, "", "stop") + mk(1, "", "stop") + "data: [DONE]\n\n"
	r, err := Aggregate(strings.NewReader(sse), WithMarkupRepair(NewMarkupRepair(nil, true)), WithExpectedChoices(2))
	if err != nil {
		t.Fatal(err)
	}
	choices := r["choices"].([]any)
	c0 := choices[0].(map[string]any)
	c1 := choices[1].(map[string]any)
	if tcs, ok := c0["message"].(map[string]any)["tool_calls"].([]any); !ok || len(tcs) != 2 {
		t.Fatalf("choice0 tool_calls = %v", c0["message"])
	}
	if c0["finish_reason"] != "tool_calls" {
		t.Fatalf("choice0 finish = %v", c0["finish_reason"])
	}
	if _, has := c1["message"].(map[string]any)["tool_calls"]; has {
		t.Fatalf("choice1 must stay text-only")
	}
	if c1["message"].(map[string]any)["content"] != "choice1 纯正文" {
		t.Fatalf("choice1 content = %v", c1["message"])
	}
}

// TestMarkupUnclosedTail：流结束时仍未判定的尾部缓冲原文交还（流式零吞字节）。
func TestMarkupUnclosedTail(t *testing.T) {
	tail := "正文" + "<" + markupPipe + "DSML" + markupPipe + " calls>没写完"
	sse := chunk(t, map[string]any{"content": tail}, nil) +
		chunk(t, map[string]any{}, "stop") + "data: [DONE]\n\n"
	w := httptest.NewRecorder()
	if err := StreamHint(w, strings.NewReader(sse), nil, WithMarkupRepair(NewMarkupRepair(nil, true))); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.Body.String(), "DSML") {
		t.Fatalf("unclosed tail must round-trip verbatim:\n%s", w.Body.String())
	}
}

// TestToolNameAllowlistSources：名单的两个来源（声明 tools / 历史调用名）。
func TestToolNameAllowlistSources(t *testing.T) {
	// 只有声明的 tools。
	only := ToolNameAllowlist([]byte(`{"tools":[{"function":{"name":"a"}},{"type":"function","name":"b"}]}`))
	if !only["a"] || !only["b"] || len(only) != 2 {
		t.Fatalf("declared tools: %v", only)
	}
	// 只有历史（tools 丢失的常态）。
	hist := ToolNameAllowlist([]byte(`{"messages":[{"role":"assistant","tool_calls":[{"function":{"name":"hist_tool"}}]}]}`))
	if !hist["hist_tool"] {
		t.Fatalf("history names: %v", hist)
	}
	// 两个来源都缺失 → nil（弱判定兜底）。
	if got := ToolNameAllowlist([]byte(`{"messages":[{"role":"user","content":"hi"}]}`)); got != nil {
		t.Fatalf("empty allowlist: %v", got)
	}
	// 解析失败按空处理。
	if got := ToolNameAllowlist([]byte(`not json`)); got != nil {
		t.Fatalf("bad json: %v", got)
	}
	// 文档入口（跨协议翻译后的 Chat 形态）。
	doc := ToolNameAllowlistDoc(map[string]any{"tools": []any{map[string]any{"function": map[string]any{"name": "doc_tool"}}}})
	if !doc["doc_tool"] {
		t.Fatalf("doc allowlist: %v", doc)
	}
}

// TestMarkupCounters：计数器语义——converted / seen / rejected 可区分
// 「没识别到」与「识别到但判定不通过」。
func TestMarkupCounters(t *testing.T) {
	repair := NewMarkupRepair(nil, true)
	m := newMarkupMachine(repair)
	m.Feed("没有任何标记的正文")
	if repair.Seen() != 0 || repair.Converted() != 0 || repair.Rejected() != 0 {
		t.Fatalf("counters should stay zero")
	}
	m2 := newMarkupMachine(repair)
	m2.Feed("<" + markupPipe + "DSML" + markupPipe + " calls><" + markupPipe + "DSML" + markupPipe + " invoke name=\"<bad>\"></" + markupPipe + "DSML" + markupPipe + " invoke></" + markupPipe + "DSML" + markupPipe + " calls>")
	if repair.Seen() != 1 || repair.Rejected() != 1 || repair.Converted() != 0 {
		t.Fatalf("rejected counter: seen=%d rejected=%d converted=%d", repair.Seen(), repair.Rejected(), repair.Converted())
	}
}

// mustJSON 复用 effort_clamp_test.go 的同名声明（同包测试文件）。
