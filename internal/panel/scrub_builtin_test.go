package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy_manager/internal/scrub"
)

// TestScrubBuiltinEndpoint 内置指纹表接口必须下发稳定 ID（面板按 ID 做启停与
// 替换覆盖），且内容与 scrub.BuiltinRules() 同源，避免"面板列的和实际跑的不一致"。
func TestScrubBuiltinEndpoint(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k"})
	req := httptest.NewRequest(http.MethodGet, "/panel/api/scrub/builtin", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Rules []scrub.BuiltinRule `json:"rules"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := scrub.BuiltinRules()
	if len(resp.Rules) != len(want) {
		t.Fatalf("规则数=%d want %d", len(resp.Rules), len(want))
	}
	seen := map[string]bool{}
	for i, r := range resp.Rules {
		if r.ID == "" || r.Match == "" {
			t.Fatalf("第 %d 条缺少 id/match: %+v", i, r)
		}
		if seen[r.ID] {
			t.Fatalf("重复 id %q", r.ID)
		}
		seen[r.ID] = true
		if r.ID != want[i].ID || r.Kind != want[i].Kind || r.Match != want[i].Match || r.Replace != want[i].Replace {
			t.Fatalf("第 %d 条与 BuiltinRules 不一致: %+v / %+v", i, r, want[i])
		}
	}
	for _, id := range []string{"err-11128", "identity-claude-agent-sdk", "hdr-billing"} {
		if !seen[id] {
			t.Fatalf("缺少关键规则 ID %q（面板无法按 ID 配置）", id)
		}
	}
}
