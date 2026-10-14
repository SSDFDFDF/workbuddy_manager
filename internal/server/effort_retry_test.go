package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"workbuddy_manager/internal/auth"
	"workbuddy_manager/internal/upstream"
)

// 每个 attempt 的回显只反映本次出站档位，不能沿用失败域的注入/钳位结果。
func TestResponsesEffortEchoAfterRealmRetry(t *testing.T) {
	wasGlobal := auth.GlobalEnabled()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(wasGlobal) })
	t.Cleanup(func() { upstream.SetEffortDefaultOverrides(nil) })

	for _, tc := range []struct {
		name          string
		model         string
		policy        string
		reasoning     any
		overrides     map[string]string
		wantSent      []string
		wantReasoning any
	}{
		{
			name: "无覆盖的成功域不回显失败域默认档", model: "glm-5.2", policy: RealmDefaultAuto,
			overrides: map[string]string{"cn:glm-5.2": "xhigh"}, wantSent: []string{"xhigh", ""},
		},
		{
			name: "保留原始 reasoning 元数据", model: "glm-5.2", policy: RealmDefaultAuto,
			reasoning: map[string]any{"summary": "auto"},
			overrides: map[string]string{"cn:glm-5.2": "xhigh"}, wantSent: []string{"xhigh", ""},
			wantReasoning: map[string]any{"summary": "auto"},
		},
		{
			name: "保留显式 null effort", model: "glm-5.2", policy: RealmDefaultAuto,
			reasoning: map[string]any{"effort": nil},
			overrides: map[string]string{"cn:glm-5.2": "xhigh"}, wantSent: []string{"xhigh", ""},
			wantReasoning: map[string]any{"effort": nil},
		},
		{
			name: "两域覆盖以成功域为准", model: "glm-5.2", policy: RealmDefaultAuto,
			overrides: map[string]string{"cn:glm-5.2": "xhigh", "global:glm-5.2": "high"},
			wantSent:  []string{"xhigh", "high"}, wantReasoning: map[string]any{"effort": "high"},
		},
		{
			name: "成功域支持原始请求档位", model: "deepseek-v4.1-flash", policy: RealmDefaultAutoGlobalCN,
			reasoning: map[string]any{"effort": "max", "summary": "auto"},
			wantSent:  []string{"high", "max"}, wantReasoning: map[string]any{"effort": "max", "summary": "auto"},
		},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.name, stream), func(t *testing.T) {
				upstream.SetEffortDefaultOverrides(tc.overrides)
				p := testPoolWith(
					&auth.Auth{UID: "cn", AccessToken: "cn-token", ExpiresAt: 9999999999, Domain: "workbuddy.cn"},
					&auth.Auth{UID: "global", AccessToken: "global-token", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"},
				)
				calls := 0
				up := newFakeUpstream(t, func(string) (int, string, bool) {
					calls++
					if calls == 1 {
						return 400, `{"code":11102,"msg":"model not supported"}`, false
					}
					return 200, sseOK, true
				})
				up.GlobalEnabled = true
				inner := up.HTTP.Transport
				var sent []string
				up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					var obj map[string]any
					if err := json.Unmarshal(raw, &obj); err != nil {
						return nil, err
					}
					effort, _ := obj["reasoning_effort"].(string)
					sent = append(sent, effort)
					r.Body = io.NopCloser(strings.NewReader(string(raw)))
					return inner.RoundTrip(r)
				})
				h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: true,
					RealmResolver: NewRealmResolver(tc.policy, nil), MaxRotate: 3})
				body := map[string]any{"model": tc.model, "store": false, "input": "hi", "stream": stream}
				if tc.reasoning != nil {
					body["reasoning"] = tc.reasoning
				}
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(raw))))
				if rec.Code != http.StatusOK || !reflect.DeepEqual(sent, tc.wantSent) {
					t.Fatalf("status=%d efforts=%v want %v body=%s", rec.Code, sent, tc.wantSent, rec.Body.String())
				}
				check := func(resp map[string]any) {
					t.Helper()
					if !reflect.DeepEqual(resp["reasoning"], tc.wantReasoning) {
						t.Errorf("echoed reasoning=%v want %v", resp["reasoning"], tc.wantReasoning)
					}
				}
				if !stream {
					var resp map[string]any
					if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
						t.Fatal(err)
					}
					check(resp)
					return
				}
				seen := map[string]bool{}
				for _, line := range strings.Split(rec.Body.String(), "\n") {
					if !strings.HasPrefix(line, "data: ") {
						continue
					}
					var event map[string]any
					if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
						t.Fatal(err)
					}
					if resp, ok := event["response"].(map[string]any); ok {
						check(resp)
						seen[event["type"].(string)] = true
					}
				}
				if !seen["response.created"] || !seen["response.completed"] {
					t.Fatalf("missing response envelopes: %s", rec.Body.String())
				}
			})
		}
	}
}
