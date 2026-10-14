package upstream

import (
	"net/http"
	"testing"

	"workbuddy_manager/internal/auth"
)

// chatHeadersFor 构造一次 chat 出站并返回请求（测试只在头层断言）。
func chatHeadersFor(t *testing.T, meta ChatMeta) http.Header {
	t.Helper()
	c := New()
	r, err := http.NewRequest("POST", "https://x.invalid/v2/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	_, _ = auth.BackfillRealmFor(a, "cn")
	c.ChatHeaders(r, a, "", meta)
	return r.Header
}

// TestAgentTypeAlwaysSent：官方 resolveAgentType() 恒发 X-Agent-Type
// （team / subagent / main）——网关缺省时也要发，并按官方同款规则派生：
// 有 parentConversationId = subagent，否则 main。
func TestAgentTypeAlwaysSent(t *testing.T) {
	if got := chatHeadersFor(t, ChatMeta{}).Get("X-Agent-Type"); got != "main" {
		t.Fatalf("default X-Agent-Type=%q want main", got)
	}
	if got := chatHeadersFor(t, ChatMeta{ParentConversationID: "p"}).Get("X-Agent-Type"); got != "subagent" {
		t.Fatalf("subagent X-Agent-Type=%q", got)
	}
	if got := chatHeadersFor(t, ChatMeta{AgentType: "team", ParentConversationID: "p"}).Get("X-Agent-Type"); got != "team" {
		t.Fatalf("passthrough X-Agent-Type=%q", got)
	}
}

// TestAgentPurposePassthrough：缺省仍是官方的 conversation（用量归因口径），
// 入站带了就透传（官方同一条链路也用 conversation_topic）。
func TestAgentPurposePassthrough(t *testing.T) {
	if got := chatHeadersFor(t, ChatMeta{}).Get("X-Agent-Purpose"); got != "conversation" {
		t.Fatalf("default X-Agent-Purpose=%q", got)
	}
	if got := chatHeadersFor(t, ChatMeta{AgentPurpose: "conversation_topic"}).Get("X-Agent-Purpose"); got != "conversation_topic" {
		t.Fatalf("passthrough X-Agent-Purpose=%q", got)
	}
}

// TestExpertHeadersPassthrough：专家模式头族只在客户端传了才发（不伪造专家身份）。
func TestExpertHeadersPassthrough(t *testing.T) {
	h := chatHeadersFor(t, ChatMeta{})
	if h.Get("X-Expert-Id") != "" || h.Get("X-Expert-Team-Task") != "" {
		t.Fatalf("expert headers must be absent by default: %v", h)
	}
	h = chatHeadersFor(t, ChatMeta{ExpertID: "SeniorDeveloper", ExpertTeamTask: "true"})
	if h.Get("X-Expert-Id") != "SeniorDeveloper" || h.Get("X-Expert-Team-Task") != "true" {
		t.Fatalf("expert headers missing: %v", h)
	}
}

// TestConversationIDStillPassthroughOnly：派生会话 ID 由 handler 决定要不要填
// ChatMeta.ConversationID；头层保持“空则不发”。
func TestConversationIDStillPassthroughOnly(t *testing.T) {
	if h := chatHeadersFor(t, ChatMeta{}); h.Get("X-Conversation-ID") != "" {
		t.Fatalf("X-Conversation-ID must be absent when empty: %v", h)
	}
	if h := chatHeadersFor(t, ChatMeta{ConversationID: "conv-1"}); h.Get("X-Conversation-ID") != "conv-1" {
		t.Fatalf("X-Conversation-ID missing: %v", h)
	}
}
