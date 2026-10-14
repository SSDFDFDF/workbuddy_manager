package forwarding

import (
	"encoding/json"
	"testing"
)

// TestVerbosityInjectedOnlyWhenThinking 对齐官方客户端的 verbosity 门控：
// 只在**思考开启**（reasoning_effort / reasoning.effort 非空，或 thinking.type=enabled）
// 且客户端没自带 verbosity 时注入 "high"；其余形态零改动（非思考请求与官方一致地不发）。
func TestVerbosityInjectedOnlyWhenThinking(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string // 期望的 verbosity（"" = 不应出现）
	}{
		{"effort 开启", `{"model":"glm-5.3","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`, "high"},
		{"camel effort 开启", `{"model":"m","reasoningEffort":"low","messages":[{"role":"user","content":"hi"}]}`, "high"},
		{"reasoning.effort 开启", `{"model":"m","reasoning":{"effort":"medium"},"messages":[{"role":"user","content":"hi"}]}`, "high"},
		{"thinking.enabled 开启", `{"model":"m","thinking":{"type":"enabled"},"messages":[{"role":"user","content":"hi"}]}`, "high"},
		{"无思考信号", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, ""},
		{"effort=off 关闭", `{"model":"m","reasoning_effort":"off","messages":[{"role":"user","content":"hi"}]}`, ""},
		{"thinking=disabled 关闭", `{"model":"m","thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"}]}`, ""},
		{"客户端自带不被覆盖", `{"model":"m","reasoning_effort":"high","verbosity":"concise","messages":[{"role":"user","content":"hi"}]}`, "concise"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Parse([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			out, err := r.Encode(r.Model)
			if err != nil {
				t.Fatal(err)
			}
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatal(err)
			}
			got, has := obj["verbosity"].(string)
			if tc.want == "" {
				if has {
					t.Fatalf("verbosity must not be injected: %v", obj["verbosity"])
				}
				return
			}
			if !has || got != tc.want {
				t.Fatalf("verbosity=%v want %q", obj["verbosity"], tc.want)
			}
			// 注入的形态必须与官方一致：思考请求同时带 reasoning_summary=auto。
			if tc.want == "high" {
				if s, _ := obj["reasoning_summary"].(string); s != "auto" {
					t.Fatalf("reasoning_summary=%v want auto", obj["reasoning_summary"])
				}
			}
		})
	}
}
