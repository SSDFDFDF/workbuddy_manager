package protocol

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"workbuddy_manager/internal/upstream"
)

// 验证 Responses 桥接路径下 DSML 还原流过 ConsumeCompletion：leaked markup → function_call。
func TestBridgeMarkupRepair(t *testing.T) {
	b, _ := os.ReadFile("../upstream/testdata/leaked_markup.txt")
	fix := string(b)
	// Responses 请求（无 tools 声明 → 弱判定）。
	req, err := Decode(Responses, []byte(`{"model":"m","input":"go"}`))
	if err != nil {
		t.Fatal(err)
	}
	esc := func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "\\", "\\\\"), "\"", "\\\"") }
	escaped := esc(fix)
	escaped = strings.ReplaceAll(escaped, "\n", "\\n")
	sse := `data: {"id":"x","model":"m","choices":[{"index":0,"delta":{"content":"` + escaped + `"}}]}` + "\n\n" +
		`data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
	w := httptest.NewRecorder()
	repair := upstream.NewMarkupRepair(nil, true)
	err = Stream(w, strings.NewReader(sse), req, nil, upstream.WithMarkupRepair(repair))
	if err != nil {
		t.Fatalf("stream: %v\n%s", err, w.Body.String())
	}
	out := w.Body.String()
	if repair.Converted() != 2 {
		t.Fatalf("converted=%d want 2:\n%s", repair.Converted(), out)
	}
	if !strings.Contains(out, "function_call") && !strings.Contains(out, "exec_command") {
		t.Fatalf("no function_call in Responses stream:\n%s", out)
	}
	if strings.Contains(out, "DSML") {
		t.Fatalf("markup leaked in bridge output:\n%s", out)
	}
}

// 非流式桥接路径：整包聚合响应里的标记同样还原成 function_call（Responses）。
func TestBridgeMarkupRepairAggregate(t *testing.T) {
	b, _ := os.ReadFile("../upstream/testdata/leaked_markup.txt")
	resp := map[string]any{
		"id": "x", "model": "m", "object": "chat.completion",
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": string(b)},
		}},
	}
	raw, _ := json.Marshal(resp)
	req, err := Decode(Responses, []byte(`{"model":"m","input":"go"}`))
	if err != nil {
		t.Fatal(err)
	}
	repair := upstream.NewMarkupRepair(nil, true)
	completion, err := Aggregate(strings.NewReader(string(raw)), req, upstream.WithMarkupRepair(repair))
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if repair.Converted() != 2 {
		t.Fatalf("converted=%d want 2", repair.Converted())
	}
	formatted, err := completion.Format(req)
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	out, _ := json.Marshal(formatted)
	if !strings.Contains(string(out), "function_call") || !strings.Contains(string(out), "exec_command") {
		t.Fatalf("no function_call in formatted Responses output: %s", out)
	}
	if strings.Contains(string(out), "DSML") {
		t.Fatalf("markup leaked into bridge output: %s", out)
	}
}

// 截断边界（FORK_CHANGES §1 第 17 条）：标记块在截断前已完整闭合 → 已还原成 calls，
// 而收尾仍是 length。各入口按自己已有的收尾一致性规则处理，本层不伪造收尾。
func TestBridgeMarkupRepairTruncatedFinish(t *testing.T) {
	b, _ := os.ReadFile("../upstream/testdata/leaked_markup.txt")
	leaked := string(b)
	snapshot := func(finish string) string {
		resp := map[string]any{
			"id": "x", "model": "m", "object": "chat.completion",
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": finish,
				"message": map[string]any{"role": "assistant", "content": leaked},
			}},
		}
		raw, _ := json.Marshal(resp)
		return string(raw)
	}
	t.Run("anthropic refuses", func(t *testing.T) {
		req, _ := Decode(Anthropic, []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"go"}]}`))
		completion, err := Aggregate(strings.NewReader(snapshot("length")), req, upstream.WithMarkupRepair(upstream.NewMarkupRepair(nil, true)))
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		// 已在正文里还原出 calls，但收尾是 length：Anthropic 不定态收尾 → 显式报错
		//（宁可大声失败，不把截断伪装成成功），而不是透出一段标记文本。
		if _, err := completion.Format(req); err == nil {
			t.Fatal("anthropic must refuse calls under a length finish")
		} else if !strings.Contains(err.Error(), "inconsistently terminated") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("responses marks incomplete", func(t *testing.T) {
		req, _ := Decode(Responses, []byte(`{"model":"m","input":"go"}`))
		completion, err := Aggregate(strings.NewReader(snapshot("length")), req, upstream.WithMarkupRepair(upstream.NewMarkupRepair(nil, true)))
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		formatted, err := completion.Format(req)
		if err != nil {
			t.Fatalf("responses format: %v", err)
		}
		out, _ := json.Marshal(formatted)
		if !strings.Contains(string(out), "\"incomplete\"") || !strings.Contains(string(out), "function_call") {
			t.Fatalf("want incomplete + function_call: %s", out)
		}
	})
}
