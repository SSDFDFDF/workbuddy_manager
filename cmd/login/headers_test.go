package main

import (
	"net/http"
	"testing"

	"workbuddy_manager/internal/upstream"
)

// TestNoAuthLoginHeaders auth/state & auth/token 的未认证请求头须带官方客户端的
// 四个 X-No-* 声明头（值 "true"），且 UA 复用 upstream.DefaultUA（与网关出站同形）。
func TestNoAuthLoginHeaders(t *testing.T) {
	for _, realm := range []string{"cn", "global"} {
		origin := originRefererCN
		if realm == realmGlobal {
			origin = originRefererGlobal
		}
		ua := upstream.DefaultUA(realm, "chat")
		req, _ := http.NewRequest(http.MethodGet, "https://x.invalid", nil)
		noAuthLoginHeaders(origin, ua)(req)

		for _, h := range []string{"X-No-Authorization", "X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
			if req.Header.Get(h) != "true" {
				t.Errorf("realm=%s %s=%q want true", realm, h, req.Header.Get(h))
			}
		}
		if req.UserAgent() != ua {
			t.Errorf("realm=%s UA=%q want %q", realm, req.UserAgent(), ua)
		}
		if req.Header.Get("Origin") != origin {
			t.Errorf("realm=%s Origin=%q want %q", realm, req.Header.Get("Origin"), origin)
		}
	}
}

// TestLoginUAMatchesGatewayDefault 登录 UA 不再硬编码旧抓包值，而是与网关默认 UA 同形。
func TestLoginUAMatchesGatewayDefault(t *testing.T) {
	if got := upstream.DefaultUA(realmCN, "chat"); got != "WorkBuddy/5.7.6 WorkBuddy/5.7.6 CLI/2.156.0" {
		t.Errorf("cn login UA=%q", got)
	}
	if got := upstream.DefaultUA(realmGlobal, "chat"); got != "WorkBuddy/5.6.2 WorkBuddy AI/5.6.2 CLI/2.147.0" {
		t.Errorf("global login UA=%q", got)
	}
}
