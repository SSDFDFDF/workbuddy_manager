package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy_manager/internal/auth"
	"workbuddy_manager/internal/reqlog"
)

// All supported error transports must provide the same uncommitted queue reply.
func TestQueueTransportContract(t *testing.T) {
	for _, status := range []int{200, 202, 400, 429} {
		for _, sse := range []bool{false, true} {
			if sse && status != 200 {
				continue
			}
			for _, codeKey := range []string{"code", "bizCode", "biz_code"} {
				for _, headerDelay := range []string{"", "60", "garbage"} {
					for _, route := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
						for _, stream := range []bool{false, true} {
							name := fmt.Sprintf("%d/sse=%v/%s/header=%s%s/stream=%v", status, sse, codeKey, headerDelay, route, stream)
							t.Run(name, func(t *testing.T) {
								payload := fmt.Sprintf(`{"%s":6021,"msg":"queued","requestId":"trace","data":{"queue_position":7,"queue_size":90,"retry_after":1500}}`, codeKey)
								raw := payload
								if sse {
									raw = "data: " + payload + "\n\n"
								}
								calls := 0
								up := newFakeUpstream(t, func(string) (int, string, bool) { calls++; return status, raw, sse })
								inner := up.HTTP.Transport
								up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
									resp, err := inner.RoundTrip(r)
									if resp != nil {
										resp.Header.Set("Retry-After", headerDelay)
									}
									return resp, err
								})
								p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
								p.SetBreaker(1, time.Hour, time.Hour)
								log := reqlog.New(reqlog.Config{})
								defer log.Close()
								h := NewHandler(Config{Pool: p, Upstream: up, RequestLog: log})
								rec := httptest.NewRecorder()
								h.ServeHTTP(rec, queueRequest(route, stream))
								wantDelay := "2"
								if headerDelay == "60" {
									wantDelay = "60"
								}
								if rec.Code != 429 || rec.Header().Get("Retry-After") != wantDelay || calls != 1 {
									t.Fatalf("status=%d retry=%q calls=%d body=%s", rec.Code, rec.Header().Get("Retry-After"), calls, rec.Body.String())
								}
								st, _ := p.Status("u1")
								if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 {
									t.Fatalf("penalty: %+v", st)
								}
								if route != "/v1/messages" {
									for _, want := range []string{`"model_queued"`, `"` + codeKey + `":6021`, "trace", "queue position 7", "queue size 90", "retry after " + wantDelay + "s"} {
										if !strings.Contains(rec.Body.String(), want) {
											t.Fatalf("missing %q: %s", want, rec.Body.String())
										}
									}
								} else if !strings.Contains(rec.Body.String(), `"rate_limit_error"`) {
									t.Fatal(rec.Body.String())
								}
								events := log.Snapshot().Recent
								if len(events) != 1 || events[0].UpstreamKind != "model_queued" || events[0].UpstreamCode != "6021" {
									t.Fatalf("events=%+v", events)
								}
							})
						}
					}
				}
			}
		}
	}
}

func queueRequest(route string, stream bool) *http.Request {
	body := map[string]any{"model": "glm-5.3", "stream": stream}
	if route == "/v1/responses" {
		body["input"] = "hi"
	} else {
		body["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}
	}
	if route == "/v1/messages" {
		body["max_tokens"] = 32
	}
	b, _ := json.Marshal(body)
	return httptest.NewRequest("POST", route, strings.NewReader(string(b)))
}

func TestQueue202WithoutCode(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 202, `{"data":{"queue_position":7,"retry_after":3}}`, false })
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, queueRequest("/v1/chat/completions", false))
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "3" || !strings.Contains(rec.Body.String(), "queue position 7") {
		t.Fatalf("%d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
}

func TestQueueAfterOutputDoesNotReplay(t *testing.T) {
	for _, route := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		t.Run(route, func(t *testing.T) {
			calls := 0
			raw := strings.SplitN(sseOK, "\n\n", 2)[0] + "\n\ndata: " + `{"bizCode":6020,"message":"queued","data":{"retry_after":1500}}` + "\n\n"
			up := newFakeUpstream(t, func(string) (int, string, bool) { calls++; return 200, raw, true })
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
			p.SetBreaker(1, time.Hour, time.Hour)
			h := NewHandler(Config{Pool: p, Upstream: up})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, queueRequest(route, true))
			if rec.Code != 200 || calls != 1 || rec.Result().Header.Get("Retry-After") != "" {
				t.Fatalf("%d %d %v", rec.Code, calls, rec.Result().Header)
			}
			want := "model_queued"
			if route == "/v1/messages" {
				want = "rate_limit_error"
			}
			if !strings.Contains(rec.Body.String(), want) {
				t.Fatal(rec.Body.String())
			}
			st, _ := p.Status("u1")
			if st.Cooling || st.ErrTotal != 0 || st.BreakerFails != 0 {
				t.Fatalf("penalty %+v", st)
			}
		})
	}
}
