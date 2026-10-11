package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"workbuddy_manager/internal/auth"
	"workbuddy_manager/internal/reqlog"
	"workbuddy_manager/internal/upstream"
)

func protocolRequest(path string, stream bool) *http.Request {
	body := fmt.Sprintf(`{"model":"cn:glm-5.2","store":false,"stream":%t,"input":"hi"}`, stream)
	if path == "/v1/messages" {
		body = fmt.Sprintf(`{"model":"cn:glm-5.2","max_tokens":100,"stream":%t,"messages":[{"role":"user","content":"hi"}]}`, stream)
	}
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Anthropic-Version", "2023-06-01")
	r.Header.Set("Authorization", "Bearer secret")
	return r
}

func TestProtocolEndpointsAndAccounting(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprint(path, stream), func(t *testing.T) {
				var calls atomic.Int32
				up := &upstream.Client{ChatBaseCN: "https://fake.example", HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls.Add(1)
					raw, _ := io.ReadAll(r.Body)
					var obj map[string]any
					if err := json.Unmarshal(raw, &obj); err != nil {
						t.Fatal(err)
					}
					if obj["model"] != "glm-5.2" || obj["stream"] != true || obj["messages"] == nil {
						t.Fatalf("wrong upstream request %s", raw)
					}
					for _, k := range []string{"input", "instructions", "store", "max_output_tokens", "system"} {
						if _, exists := obj[k]; exists {
							t.Fatalf("foreign field %s in %s", k, raw)
						}
					}
					if r.Header.Get("Authorization") != "Bearer at1" || r.Header.Get("X-Api-Key") != "" {
						t.Fatal("downstream credential leaked")
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sseOK))}, nil
				})}}
				logs := reqlog.New(reqlog.Config{})
				p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
				h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "secret", RequestLog: logs})
				r := protocolRequest(path, stream)
				if path == "/v1/messages" {
					r.Header.Del("Authorization")
					r.Header.Set("X-Api-Key", "secret")
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, r)
				if rec.Code != 200 {
					t.Fatalf("%d %s", rec.Code, rec.Body)
				}
				if !strings.Contains(rec.Body.String(), "你好") || !strings.Contains(rec.Body.String(), "cn:glm-5.2") {
					t.Fatal(rec.Body.String())
				}
				if stream && !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
					t.Fatal(rec.Header())
				}
				if strings.Contains(rec.Body.String(), "[DONE]") || strings.Contains(rec.Body.String(), "chat.completion") {
					t.Fatal("native response leaked")
				}
				if calls.Load() != 1 {
					t.Fatal(calls.Load())
				}
				s := logs.Snapshot()
				if len(s.Recent) != 1 || s.Recent[0].Path != path || !s.Recent[0].OK || s.Recent[0].Attempts != 1 || s.Recent[0].TotalTokens != 2 {
					t.Fatalf("wrong accounting: %+v", s.Recent)
				}
			})
		}
	}
}

func TestMessagesAuthAndValidation(t *testing.T) {
	// version/beta headers are informational and ignored (see protocol.go);
	// only credential conflicts and duplicates fail.
	cases := []struct {
		name, key, bearer, version, beta string
		code                             int
	}{
		{"x-key", "secret", "", "2023-06-01", "", 200},
		{"bearer", "", "Bearer secret", "2023-06-01", "", 200},
		{"both", "secret", "Bearer secret", "2023-06-01", "", 200},
		{"conflict", "wrong", "Bearer secret", "2023-06-01", "", 401},
		{"conflict2", "secret", "Bearer wrong", "2023-06-01", "", 401},
		{"missing", "", "", "2023-06-01", "", 401},
		{"any-version", "secret", "", "future", "", 200},
		{"no-version", "secret", "", "", "", 200},
		{"beta-header", "secret", "", "2023-06-01", "tools-beta", 200},
		{"duplicate-key", "secret", "", "2023-06-01", "", 401},
		{"duplicate-bearer", "", "Bearer secret", "2023-06-01", "", 401},
		{"duplicate-version", "secret", "", "2023-06-01", "", 200},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var called int
			up := newFakeUpstream(t, func(string) (int, string, bool) { called++; return 200, sseOK, true })
			h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u", AccessToken: "a", ExpiresAt: 9999999999}), Upstream: up, APIKey: "secret"})
			r := protocolRequest("/v1/messages", false)
			r.Header.Del("Authorization")
			r.Header.Del("Anthropic-Version")
			if tt.key != "" {
				r.Header.Set("X-Api-Key", tt.key)
			}
			if tt.bearer != "" {
				r.Header.Set("Authorization", tt.bearer)
			}
			if tt.version != "" {
				r.Header.Set("Anthropic-Version", tt.version)
			}
			if tt.beta != "" {
				r.Header.Set("Anthropic-Beta", tt.beta)
			}
			switch tt.name {
			case "duplicate-key":
				r.Header.Add("X-Api-Key", "secret")
			case "duplicate-bearer":
				r.Header.Add("Authorization", "Bearer secret")
			case "duplicate-version":
				r.Header.Add("Anthropic-Version", "2023-06-01")
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != tt.code {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
			if tt.code != 200 && (called != 0 || !strings.Contains(rec.Body.String(), `"type":"error"`)) {
				t.Fatalf("invalid auth error: %s calls %d", rec.Body, called)
			}
		})
	}
}

func TestProtocolNoReplayOnTruncation(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			var calls int
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				calls++
				return 200, `data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}` + "\n\n", true
			})
			logs := reqlog.New(reqlog.Config{})
			h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "a1", ExpiresAt: 9999999999}, &auth.Auth{UID: "u2", AccessToken: "a2", ExpiresAt: 9999999999}), Upstream: up, RequestLog: logs})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, protocolRequest(path, stream))
			if calls != 1 {
				t.Fatalf("replayed %d calls", calls)
			}
			if !stream && rec.Code != 502 {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
			if stream && !strings.Contains(rec.Body.String(), "error") {
				t.Fatal(rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "event: response.completed") || strings.Contains(rec.Body.String(), "event: message_stop") {
				t.Fatal("false successful terminal")
			}
			s := logs.Snapshot()
			if len(s.Recent) != 1 || s.Recent[0].OK || s.Recent[0].Attempts != 1 {
				t.Fatal(s.Recent)
			}
		}
	}
}

func TestCodexStyleResponsesRequestAccepted(t *testing.T) {
	// Codex sends reasoning history, include, client_metadata and other hints on
	// every request. They must be accepted, never forwarded, and the response
	// reasoning text must not leak into the client body.
	sse := `data: {"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"secret thoughts "}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"content":"你好"},"finish_reason":"stop"}]}` + "\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":1}}` + "\n\ndata: [DONE]\n\n"
	up := &upstream.Client{ChatBaseCN: "https://fake.example", HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		var obj map[string]any
		if err := json.Unmarshal(body, &obj); err != nil {
			t.Fatal(err)
		}
		// cn:glm-5.2 的档位能力是 [high xhigh]（静态兜底表），客户端要的 medium 会被
		// 归一到 ≤medium 的最高支持档 high：上游对越界档位回 400 code=11133，而换号无用
		// （账号与 body 无关），只能在发送前归一。
		if obj["reasoning_effort"] != "high" {
			t.Fatalf("effort not clamped to model capability: %s", body)
		}
		msgs, _ := obj["messages"].([]any)
		if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" {
			t.Fatalf("reasoning history not dropped: %s", body)
		}
		// prompt_cache_key 现在被桥提升为 Chat 顶层字段（粘性键 + 上游前缀缓存，
		// 与原生 Chat 透传口径一致）：不再断言它被丢弃。
		for _, k := range []string{"client_metadata", "service_tier", "include", "reasoning", "truncation"} {
			if _, exists := obj[k]; exists {
				t.Fatalf("dropped field %s forwarded: %s", k, body)
			}
		}
		if obj["prompt_cache_key"] != "k" {
			t.Fatalf("prompt_cache_key should reach upstream (hoisted by bridge): %s", body)
		}
		// stream_options is the encoder's own include_usage contract, not the
		// client's dropped value.
		if so, _ := obj["stream_options"].(map[string]any); so["include_usage"] != true {
			t.Fatalf("wrong stream_options: %s", body)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sse))}, nil
	})}}
	logs := reqlog.New(reqlog.Config{})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up, APIKey: "secret", RequestLog: logs})
	body := `{"model":"cn:glm-5.2","store":false,"stream":true,"input":[` +
		`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"zzz"},` +
		`{"role":"user","content":"hi"}],` +
		`"include":["reasoning.encrypted_content"],"client_metadata":{"x":"1"},` +
		`"service_tier":"priority","prompt_cache_key":"k","stream_options":{"include_usage":true},` +
		`"text":{"verbosity":"low"},"reasoning":{"effort":"medium","summary":"auto"}}`
	r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "你好") || strings.Contains(out, "secret thoughts") {
		t.Fatal(out)
	}
	// 归档事件要能回答“客户端发了什么被我们丢了”。
	s := logs.Snapshot()
	if len(s.Recent) != 1 {
		t.Fatalf("events=%d", len(s.Recent))
	}
	want := []string{"client_metadata", "service_tier", "stream_options", "include", "text.verbosity", "reasoning.summary", "input[]"}
	if !reflect.DeepEqual(s.Recent[0].Dropped, want) {
		t.Fatalf("dropped=%v want %v", s.Recent[0].Dropped, want)
	}
	// 改写与丢弃分开记（换了个值执行了 vs 没执行）：上面这串遥测字段就有 7 项，
	// 共用列表时第 9 项会被 maxDroppedFields 静默截断，最需要被看见的改写会消失。
	wantRewritten := []string{"reasoning.effort:medium→high"}
	if !reflect.DeepEqual(s.Recent[0].Rewritten, wantRewritten) {
		t.Fatalf("rewritten=%v want %v", s.Recent[0].Rewritten, wantRewritten)
	}
}

func TestAnthropicStopSequencesReachUpstream(t *testing.T) {
	// The translated Chat `stop` field is an extension Parse must preserve: it
	// is not a declared forwarding field, only the outbound body proves it.
	up := &upstream.Client{ChatBaseCN: "https://fake.example", HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		var obj map[string]any
		if err := json.Unmarshal(body, &obj); err != nil {
			t.Fatal(err)
		}
		stop, _ := obj["stop"].([]any)
		if len(stop) != 2 || stop[0] != "</block>" || stop[1] != "END" {
			t.Fatalf("stop not forwarded: %s", body)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sseOK))}, nil
	})}}
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up, APIKey: "secret"})
	body := `{"model":"cn:glm-5.2","max_tokens":100,"stream":false,"messages":[{"role":"user","content":"hi"}],"stop_sequences":["</block>","END"]}`
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	r.Header.Set("X-Api-Key", "secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestUnsupportedProtocolInputNeverReachesUpstream(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { t.Fatal("invalid request reached upstream"); return 200, sseOK, true })
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u", AccessToken: "a", ExpiresAt: 9999999999}), Upstream: up})
	for path, body := range map[string]string{
		"/v1/responses": `{"model":"x","input":"hi","store":false,"previous_response_id":"other-user-response"}`,
		"/v1/messages":  `{"model":"x","max_tokens":0,"messages":[{"role":"user","content":"hi"}]}`,
	} {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Anthropic-Version", "2023-06-01")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != 400 {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	}
}
