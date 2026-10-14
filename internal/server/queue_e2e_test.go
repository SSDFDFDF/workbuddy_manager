package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy_manager/internal/auth"
	"workbuddy_manager/internal/upstream"
)

// TestModelQueuedEndToEnd 端到端契约（FORK_CHANGES §7：上游容量队列）：
//   - 分类为 model_queued、HTTP 429（可重试语义）；
//   - Retry-After 来自 body 的 data.retry_after（毫秒口径归一成秒）；202 之外的
//     「429 + 6022/6021 + 文案含 limit」也不得被限流词表抢走；
//   - 保留安全业务字段 + gateway_hint 带位置/容量/等待时长（不透传任意原始 body）；
//   - **不罚账号**：不冷却、不熔断（健康号必须留在池内）。
func TestModelQueuedEndToEnd(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return http.StatusTooManyRequests,
			`{"code":6021,"msg":"model queue is full, limit reached","data":{"queue_position":7,"queue_size":90,"retry_after":1500}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// 熔断阈值 1：若误罚 NoteError 一次即熔断；队列错误不应喂熔断。
	p.SetBreaker(1, time.Hour, time.Hour)
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"排队"}]}`)))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429 (body=%s)", rec.Code, rec.Body.String())
	}
	// 1500ms → 2s（向上取整；整秒值不额外加一）。
	if got := rec.Header().Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After=%q want 2 (body=%s)", got, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"6021", "queue is full", "gateway_hint", "queue position 7", "queue size 90"} {
		if !strings.Contains(body, want) {
			t.Fatalf("响应缺少 %q: %s", want, body)
		}
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Error.Code != "model_queued" {
		t.Fatalf("error.code=%q want model_queued", env.Error.Code)
	}
	if st, _ := p.Status("u1"); st.Cooling || st.Disabled || st.ErrTotal != 0 {
		t.Fatalf("队列错误不应罚账号: %+v", st)
	}
}

// TestModelQueuedDoesNotRotate 队列**不轮转**：6020 是模型全局容量，换号不缩队，
// 且白耗健康号额并丢掉会话缓存。上游只应被调用一次。
func TestModelQueuedDoesNotRotate(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return http.StatusBadRequest, `{"code":6020,"msg":"queued","data":{"retry_after":3}}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"排队"}]}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429", rec.Code)
	}
	if calls != 1 {
		t.Fatalf("上游被调用 %d 次；队列错误不应轮转", calls)
	}
	if got := rec.Header().Get("Retry-After"); got != "3" {
		t.Fatalf("Retry-After=%q want 3（整秒不额外加一）", got)
	}
}

// TestModelQueuedArchived：归档记 upstream_kind/upstream_code（否则队列出现频率
// 无法离线统计——这正是本次观测缺口的由来）。
func TestModelQueuedArchived(t *testing.T) {
	st := newChatStat(time.Now(), "glm-5.3", false)
	payload := `{"code":6021,"msg":"full"}`
	kind := upstream.Classify(http.StatusTooManyRequests, payload)
	st.noteUpstreamError(kind.String(), upstream.BusinessCode(payload))
	e := (&requestTrace{start: time.Now(), id: "r", stat: st}).event(http.StatusTooManyRequests)
	if e.UpstreamKind != "model_queued" || e.UpstreamCode != "6021" {
		t.Fatalf("归档字段 kind=%q code=%q", e.UpstreamKind, e.UpstreamCode)
	}
}

// TestHTTP202QueuedBecomesRetryable：上游 202（已受理/排队）此前是 502
// upstream_queue_unsupported（客户端拿到一个不存在的上游故障）；现应 429 + 可重试，
// 且队列业务码作为安全诊断字段返回给客户端。
func TestHTTP202QueuedBecomesRetryable(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return http.StatusAccepted, `{"code":6020,"msg":"queued","data":{"queue_position":2,"retry_after":2}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"排队"}]}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model_queued") {
		t.Fatalf("code 应为 model_queued: %s", rec.Body.String())
	}
	if st, _ := p.Status("u1"); st.Cooling || st.Disabled || st.ErrTotal != 0 {
		t.Fatalf("202 排队不应罚账号: %+v", st)
	}
}
