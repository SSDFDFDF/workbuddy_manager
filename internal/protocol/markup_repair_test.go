package protocol

import (
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
