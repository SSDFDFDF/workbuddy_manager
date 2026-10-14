package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy_manager/internal/auth"
	"workbuddy_manager/internal/upstream"
)

// headerCapture 捕获 handler 真正发给上游的请求头。
type headerCapture struct {
	hdr  http.Header
	seen bool
}

// headerCaptureHandler 与 captureHandler 同构，但记录出站请求头
// （会话身份出站策略的断言对象是头，不是 body）。
func headerCaptureHandler(t *testing.T, cap *headerCapture) *Handler {
	t.Helper()
	base := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, gatewayBenchSSE, true
	})
	inner := base.HTTP.Transport
	base.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		cap.hdr = r.Header.Clone()
		cap.seen = true
		return inner.RoundTrip(r)
	})
	return NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: base,
	})
}

// sendChat 打一次 chat 请求并返回响应用于断言成功路径。
func sendChat(t *testing.T, h *Handler, body string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestConversationIDNotDerivedByDefault：默认（配置关）保持历史行为——客户端没给
// 会话 id 就不发 X-Conversation-ID（不伪造），acp-connection-id 仍按既有逻辑兜底。
func TestConversationIDNotDerivedByDefault(t *testing.T) {
	cap := &headerCapture{}
	h := headerCaptureHandler(t, cap)
	sendChat(t, h, `{"model":"glm-5.3","messages":[{"role":"user","content":"默认不派生"}]}`)
	if !cap.seen {
		t.Fatal("未捕获到出站请求")
	}
	if got := cap.hdr.Get("X-Conversation-ID"); got != "" {
		t.Fatalf("X-Conversation-ID=%q want absent", got)
	}
	if cap.hdr.Get("acp-connection-id") == "" {
		t.Fatal("acp-connection-id 缺失（既有兜底逻辑被破坏）")
	}
	if got := cap.hdr.Get("X-Agent-Type"); got != "main" {
		t.Fatalf("X-Agent-Type=%q want main", got)
	}
}

// TestDerivedConversationIDEnabled：开关开启且客户端没给会话 id 时，出站带上
// 网关派生的 UUIDv4 会话身份；同一会话（同 system + 同首条 user → 同派生键）
// 多轮恒定；不同会话互异；客户端自带会话 id 时仍以客户端为准（不派生、不改写）。
func TestDerivedConversationIDEnabled(t *testing.T) {
	cap := &headerCapture{}
	h := headerCaptureHandler(t, cap)
	if h.cfg.Upstream == nil {
		t.Fatal("无 upstream client")
	}
	h.cfg.Upstream.Configure(upstream.Options{DeriveConversationID: true})

	body := `{"model":"glm-5.3","messages":[{"role":"system","content":"sys"},{"role":"user","content":"第一轮"}]}`
	sendChat(t, h, body)
	first := cap.hdr.Get("X-Conversation-ID")
	if first == "" {
		t.Fatal("X-Conversation-ID 未派生")
	}
	if len(first) != 36 || first[14] != '4' || first[8] != '-' || first[13] != '-' {
		t.Fatalf("派生会话 id 形态不对: %q", first)
	}

	// 同一会话（多轮追加历史，system/首条 user 不变）→ 同值。
	sendChat(t, h, `{"model":"glm-5.3","messages":[{"role":"system","content":"sys"},{"role":"user","content":"第一轮"},{"role":"assistant","content":"回答"},{"role":"user","content":"第二轮"}]}`)
	if got := cap.hdr.Get("X-Conversation-ID"); got != first {
		t.Fatalf("同一会话派生值漂移: %q vs %q", got, first)
	}

	// 不同会话 → 不同值。
	sendChat(t, h, `{"model":"glm-5.3","messages":[{"role":"system","content":"sys"},{"role":"user","content":"另一个问题"}]}`)
	if got := cap.hdr.Get("X-Conversation-ID"); got == first {
		t.Fatalf("不同会话派生值相同: %q", got)
	}

	// 客户端自带 → 透传优先（不派生覆盖）。
	sendChat(t, h, `{"model":"glm-5.3","conversation_id":"client-conv","messages":[{"role":"user","content":"任意"}]}`)
	if got := cap.hdr.Get("X-Conversation-ID"); got != "client-conv" {
		t.Fatalf("客户端会话 id 未被透传: %q", got)
	}
}

// TestDerivedConversationIDArchived：派生事实入归档（A/B 与排障要能区分
// “客户端给的会话身份”与“网关派生”）。
func TestDerivedConversationIDArchived(t *testing.T) {
	cap := &headerCapture{}
	h := headerCaptureHandler(t, cap)
	h.cfg.Upstream.Configure(upstream.Options{DeriveConversationID: true})
	st := newChatStat(time.Now(), "glm-5.3", true)
	st.conversationDerived = true
	e := (&requestTrace{start: time.Now(), id: "r", stat: st}).event(http.StatusOK)
	if !e.ConversationDerived {
		t.Fatal("conversation_derived 未写入归档事件")
	}
}
