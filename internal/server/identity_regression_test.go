package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy_manager/internal/upstream"
)

func TestDerivedIdentityIsolation(t *testing.T) {
	seen := map[string]bool{}
	for _, principal := range []string{"tenant-a", "tenant-b"} {
		cap := &headerCapture{}
		h := headerCaptureHandler(t, cap)
		h.cfg.APIKey = principal
		h.cfg.Upstream.CacheSecret = []byte(strings.Repeat("x", 32))
		h.cfg.Upstream.Configure(upstream.Options{DeriveConversationID: true})
		var sent map[string]any
		inner := h.cfg.Upstream.HTTP.Transport
		h.cfg.Upstream.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			b, err := io.ReadAll(r.Body)
			if err != nil {
				return nil, err
			}
			r.Body = io.NopCloser(strings.NewReader(string(b)))
			if err := json.Unmarshal(b, &sent); err != nil {
				return nil, err
			}
			return inner.RoundTrip(r)
		})
		for _, system := range []any{"You are a translator", "You are an accountant", []any{map[string]any{"type": "text", "text": "block system"}}} {
			body := map[string]any{"model": "glm-5.3", "system": system, "max_tokens": 32, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
			send := func() {
				raw, _ := json.Marshal(body)
				r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(string(raw)))
				r.Header.Set("Authorization", "Bearer "+principal)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, r)
				if rec.Code != 200 {
					t.Fatal(rec.Body.String())
				}
			}
			send()
			id := cap.hdr.Get("X-Conversation-ID")
			cache, _ := sent["prompt_cache_key"].(string)
			if id == "" || cache == "" || seen[id] || seen[cache] {
				t.Fatalf("identity collision: id=%s cache=%s", id, cache)
			}
			seen[id], seen[cache] = true, true
			body["messages"] = append(body["messages"].([]any), map[string]any{"role": "assistant", "content": "ok"}, map[string]any{"role": "user", "content": "next"})
			send()
			if cap.hdr.Get("X-Conversation-ID") != id || sent["prompt_cache_key"] != cache {
				t.Fatal("appended history changed identity")
			}
		}
	}
}

func TestDerivedIdentityExplicitBodyKeys(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields func(string) map[string]any
	}{
		{"session", func(id string) map[string]any { return map[string]any{"session_id": id} }},
		{"thread", func(id string) map[string]any { return map[string]any{"thread_id": id} }},
		{"claude-session", func(id string) map[string]any {
			return map[string]any{"metadata": map[string]any{"user_id": `{"session_id":"` + id + `"}`}}
		}},
		{"user-and-session", func(id string) map[string]any {
			return map[string]any{"session_id": id, "metadata": map[string]any{"user_id": "alice"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cap := &headerCapture{}
			h := headerCaptureHandler(t, cap)
			h.cfg.Upstream.Configure(upstream.Options{DeriveConversationID: true})
			send := func(sessionID, content string) string {
				body := tc.fields(sessionID)
				body["model"] = "glm-5.3"
				body["messages"] = []any{map[string]any{"role": "user", "content": content}}
				raw, _ := json.Marshal(body)
				sendChat(t, h, string(raw))
				return cap.hdr.Get("X-Conversation-ID")
			}
			first := send("session-A", "hi")
			if second := send("session-B", "hi"); first == "" || second == "" || second == first {
				t.Fatalf("explicit session collision: A=%q B=%q", first, second)
			}
			if changed := send("session-A", "different opening"); changed != first {
				t.Fatalf("content overrode explicit session: %q != %q", changed, first)
			}
		})
	}
}

func TestDerivedIdentityToggleAndUserIDs(t *testing.T) {
	cap := &headerCapture{}
	h := headerCaptureHandler(t, cap)
	body := `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`
	for _, enabled := range []bool{false, true, false} {
		h.cfg.Upstream.Configure(upstream.Options{DeriveConversationID: enabled})
		sendChat(t, h, body)
		if (cap.hdr.Get("X-Conversation-ID") != "") != enabled {
			t.Fatal("toggle not applied", cap.hdr)
		}
		if cap.hdr.Get("acp-connection-id") == "" {
			t.Fatal("ACP always present")
		}
	}
	h.cfg.Upstream.Configure(upstream.Options{DeriveConversationID: true})
	for _, tc := range []struct {
		extra string
		want  bool
	}{
		{`,"metadata":{"user_id":"alice"}`, false},
		{`,"metadata":{"user_id":"user_a1b2c3_session_550e8400-e29b-41d4-a716-446655440000"}`, true},
		{`,"metadata":{"user_id":"alice"},"prompt_cache_key":"explicit"`, true},
	} {
		sendChat(t, h, strings.TrimSuffix(body, "}")+tc.extra+"}")
		if (cap.hdr.Get("X-Conversation-ID") != "") != tc.want {
			t.Fatal(tc, cap.hdr)
		}
	}
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("X-Conversation-ID", "client-original")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 200 || cap.hdr.Get("X-Conversation-ID") != "client-original" {
		t.Fatal("explicit id overwritten")
	}
}
