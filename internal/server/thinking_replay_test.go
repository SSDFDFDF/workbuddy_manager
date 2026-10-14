package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"workbuddy_manager/internal/auth"
	"workbuddy_manager/internal/reqlog"
	"workbuddy_manager/internal/scrub"
	"workbuddy_manager/internal/upstream"
)

// Fill the dropped-list cap before reasoning to prove counters are independent.
func replayTestBody(stream bool) string {
	blocks := make([]any, 0, 23)
	for i := 0; i < 9; i++ {
		blocks = append(blocks, map[string]any{"type": "custom_" + strconv.Itoa(i)})
	}
	for i := 0; i < 12; i++ {
		blocks = append(blocks, map[string]any{"type": "thinking", "thinking": " 私密\n", "signature": "private-signature"})
	}
	blocks = append(blocks, map[string]any{"type": "redacted_thinking", "data": "private-opaque"}, map[string]any{"type": "text", "text": "ok"})
	b, _ := json.Marshal(map[string]any{"model": "glm-5.3", "max_tokens": 32, "stream": stream, "messages": []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": blocks},
		map[string]any{"role": "user", "content": "continue"},
	}})
	return string(b)
}

func TestThinkingReplayOutboundHotSwitchAndArchive(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(strconv.FormatBool(stream), func(t *testing.T) {
			log := reqlog.New(reqlog.Config{Dir: t.TempDir(), Enabled: true})
			defer log.Close()
			cap := &outboundCapture{}
			h := captureHandler(t, Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), RequestLog: log}, cap)
			inner := h.cfg.Upstream.HTTP.Transport
			h.cfg.Upstream.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				resp, err := inner.RoundTrip(r)
				if err == nil {
					// A genuine upstream reasoning delta still must not become a
					// signed Messages block, in either sync or streaming mode.
					_ = resp.Body.Close()
					resp.Body = io.NopCloser(strings.NewReader(`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"upstream-private-thoughts"}}]}` + "\n\n" + gatewayBenchSSE))
				}
				return resp, err
			})
			for i, enabled := range []bool{false, true, false} {
				if i > 0 { // first request tests the unconfigured default, too
					h.cfg.Upstream.Configure(upstream.Options{ReplayThinking: enabled})
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(replayTestBody(stream))))
				if rec.Code != http.StatusOK || !cap.seen {
					t.Fatal(rec.Code, rec.Body.String())
				}
				var sent map[string]any
				if err := json.Unmarshal([]byte(cap.body), &sent); err != nil {
					t.Fatal(err)
				}
				assistant := sent["messages"].([]any)[1].(map[string]any)
				for _, field := range []string{"reasoning", "reasoning_content"} {
					if enabled && assistant[field] != strings.Repeat(" 私密\n", 12) {
						t.Fatal("outbound plaintext changed", field, assistant)
					}
					if !enabled && assistant[field] != nil {
						t.Fatal("off still replayed", field)
					}
				}
				for _, secret := range []string{"private-signature", "private-opaque"} {
					if strings.Contains(cap.body, secret) {
						t.Fatal("opaque/signature data forwarded")
					}
				}
				if strings.Contains(rec.Body.String(), `"type":"thinking"`) || strings.Contains(rec.Body.String(), "signature") || strings.Contains(rec.Body.String(), "upstream-private-thoughts") {
					t.Fatal("response thinking/signatures must not be invented")
				}
			}
			log.Close()
			rows, err := log.ReadArchive(10, reqlog.Filter{})
			if err != nil || len(rows) != 3 {
				t.Fatal(len(rows), err)
			}
			on := 0
			for _, e := range rows {
				if e.ThinkingPlainBlocks != 12 || e.ThinkingRedactedBlocks != 1 {
					t.Fatalf("counters truncated with dropped fields: %+v", e)
				}
				if e.ThinkingReplayEnabled {
					on++
					if e.ThinkingReplayedBlocks != 12 || e.ThinkingReplayedBytes != 12*len(" 私密\n") {
						t.Fatal(e)
					}
				} else if e.ThinkingReplayedBlocks != 0 || e.ThinkingReplayedBytes != 0 {
					t.Fatal(e)
				}
				if len(e.Dropped) != 8 {
					// dropped 列表被 9 个 custom_* 块填满（上限 8）：这里只验证计数不受截断影响；
					// thinking/redacted 是否进 dropped 由 protocol 层用例精确断言。
					t.Fatal("independent counters must survive dropped truncation", e.Dropped)
				}
				b, _ := json.Marshal(e)
				for _, secret := range []string{"私密", "private-signature", "private-opaque", "upstream-private-thoughts"} {
					if strings.Contains(string(b), secret) {
						t.Fatal("reasoning/signature leaked into archive")
					}
				}
			}
			if on != 1 {
				t.Fatal("hot-switch state not archived", on)
			}
		})
	}
}

func TestThinkingReplayRetryKeepsRequestPolicy(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		if calls == 1 {
			return 400, `{"code":11102,"msg":"service info not found"}`, false
		}
		return 200, sseOK, true
	})
	up.Configure(upstream.Options{ReplayThinking: true})
	inner := up.HTTP.Transport
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		var obj map[string]any
		if err := json.Unmarshal(b, &obj); err != nil {
			return nil, err
		}
		if obj["messages"].([]any)[1].(map[string]any)["reasoning_content"] != strings.Repeat(" 私密\n", 12) {
			t.Error("retry re-decoded history with a different policy")
		}
		// Changing config after decoding must only affect the NEXT request.
		up.Configure(upstream.Options{ReplayThinking: false})
		r.Body = io.NopCloser(strings.NewReader(string(b)))
		return inner.RoundTrip(r)
	})
	log := reqlog.New(reqlog.Config{})
	defer log.Close()
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}, &auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999}), Upstream: up, RequestLog: log})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(replayTestBody(false))))
	if rec.Code != 200 || calls != 2 {
		t.Fatal(rec.Code, calls, rec.Body.String())
	}
	events := log.Snapshot().Recent
	if len(events) != 1 || !events[0].ThinkingReplayEnabled || events[0].ThinkingReplayedBlocks != 12 {
		t.Fatal("retries must not multiply input counters", events)
	}
}

func TestThinkingReplayUsesExistingFingerprintPolicy(t *testing.T) {
	up := &upstream.Client{}
	up.Fingerprints.Store(scrub.NewLayer(true, nil))
	cap := &outboundCapture{}
	h := captureHandler(t, Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up}, cap)
	h.cfg.Upstream.Configure(upstream.Options{ReplayThinking: true})
	raw := `{"model":"glm-5.3","max_tokens":16,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"` + auditFingerprint + `"},{"type":"text","text":"ok"}]}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(raw)))
	if rec.Code != 200 || strings.Contains(cap.body, auditFingerprint) || !strings.Contains(cap.body, "official CLI tool for Claude") {
		t.Fatal("replay bypassed existing fingerprint policy", rec.Code, cap.body)
	}
}
