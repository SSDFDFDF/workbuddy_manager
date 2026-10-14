package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"workbuddy_manager/internal/auth"
	"workbuddy_manager/internal/reqlog"
)

func TestResponsesVerbosityOutbound(t *testing.T) {
	for _, effort := range []string{"high", "none"} {
		for _, v := range []string{"", "low", "medium", "high"} {
			t.Run(effort+"/"+v, func(t *testing.T) {
				cap := &headerCapture{}
				h := headerCaptureHandler(t, cap)
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
				req := map[string]any{"model": "glm-5.3", "input": "hi", "reasoning": map[string]any{"effort": effort}}
				if v != "" {
					req["text"] = map[string]any{"verbosity": v}
				}
				raw, _ := json.Marshal(req)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(raw))))
				if rec.Code != 200 {
					t.Fatal(rec.Body.String())
				}
				want := v
				if want == "" && effort == "high" {
					want = "high"
				}
				got, _ := sent["verbosity"].(string)
				if got != want {
					t.Fatalf("verbosity=%q want=%q", got, want)
				}
			})
		}
	}
}

func TestUnknownBlockArchiveRemainsReadable(t *testing.T) {
	log := reqlog.New(reqlog.Config{Dir: t.TempDir(), Enabled: true})
	defer log.Close()
	cap := &headerCapture{}
	h := headerCaptureHandler(t, cap)
	// NewHandler wires the tracing middleware at construction, so use its fake client.
	h = NewHandler(Config{Pool: h.cfg.Pool, Upstream: h.cfg.Upstream, RequestLog: log})
	for _, typ := range []string{strings.Repeat("x", 1<<20), "line\nbreak", "short_custom"} {
		raw, _ := json.Marshal(map[string]any{"model": "glm-5.3", "max_tokens": 32, "messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hi"}, map[string]any{"type": typ}}}}})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(string(raw))))
		if rec.Code != 200 {
			t.Fatal(rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, queueRequest("/v1/messages", false))
	log.Close() // drains the archive writer
	rows, err := log.ReadArchive(10, reqlog.Filter{})
	if err != nil || len(rows) != 4 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	for _, e := range rows {
		for _, d := range e.Dropped {
			if len(d) > maxDroppedFieldBytes || strings.Contains(d, "\n") {
				t.Fatalf("unbounded diagnostic: len=%d", len(d))
			}
		}
	}
	// Independent byte cap also covers arbitrary field names from other protocols.
	for _, d := range droppedFields([]string{strings.Repeat("中", 1000)}) {
		if len(d) > maxDroppedFieldBytes || !utf8.ValidString(d) {
			t.Fatal("invalid bounded UTF8")
		}
	}
}

func TestFinalAttemptErrorArchive(t *testing.T) {
	for _, last := range []string{"queue", "success", "codeless"} {
		for _, stream := range []bool{false, true} {
			t.Run(last+map[bool]string{true: "/stream", false: "/sync"}[stream], func(t *testing.T) {
				calls := 0
				up := newFakeUpstream(t, func(string) (int, string, bool) {
					calls++
					if calls == 1 {
						return 400, `{"code":11102,"msg":"service info not found"}`, false
					}
					switch last {
					case "queue":
						return 400, `{"code":6020,"msg":"queued"}`, false
					case "codeless":
						return 400, `{"msg":"invalid request"}`, false
					default:
						return 200, sseOK, true
					}
				})
				log := reqlog.New(reqlog.Config{})
				defer log.Close()
				h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}, &auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999}), Upstream: up, RequestLog: log})
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, queueRequest("/v1/chat/completions", stream))
				events := log.Snapshot().Recent
				if calls != 2 || len(events) != 1 {
					t.Fatal(calls, events)
				}
				wantKind, wantCode := "", ""
				switch last {
				case "queue":
					wantKind, wantCode = "model_queued", "6020"
				case "codeless":
					wantKind = "client"
				}
				e := events[0]
				if e.UpstreamKind != wantKind || e.UpstreamCode != wantCode {
					t.Fatalf("last=%s archive=%+v", last, e)
				}
			})
		}
	}
}
