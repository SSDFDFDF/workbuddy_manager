package upstream

import (
	"net/http"
	"strings"
	"testing"

	"workbuddy_manager/internal/auth"
)

func TestV2RealmIdentity(t *testing.T) {
	for _, realm := range []string{"cn", "global"} {
		a := &auth.Auth{UID: realm}
		_, _ = auth.BackfillRealmFor(a, realm)
		c := New()
		want := "WorkBuddy/5.7.6 WorkBuddy/5.7.6 CLI/2.156.0"
		ver := "5.7.6"
		if realm == "global" {
			want = "WorkBuddy/5.6.2 WorkBuddy AI/5.6.2 CLI/2.147.0"
			ver = "5.6.2"
		}
		for _, host := range []string{"https://www.workbuddy.cn", "https://www.workbuddy.ai", "https://proxy.invalid"} {
			r, _ := http.NewRequest("POST", host, nil)
			c.ChatHeaders(r, a, "", ChatMeta{})
			if r.UserAgent() != want || r.Header.Get("X-IDE-Version") != ver {
				t.Fatal(r.Header)
			}
			if r.Header.Get("X-Refresh-Token") != "" {
				t.Fatal("refresh leaked")
			}
		}
		r, _ := http.NewRequest("GET", "https://billing.invalid", nil)
		c.BillingHeaders(r, a)
		if r.UserAgent() != want {
			t.Fatal(r.Header)
		}
	}
}
func TestV2PurposeOverride(t *testing.T) {
	c := New()
	c.Profiles = map[string]IdentityProfile{"global": {ClientVersion: "6.0.0", CLIVersion: "3.0.0", UserAgents: map[string]string{"chat": "explicit/1"}}}
	a := &auth.Auth{}
	_, _ = auth.BackfillRealmFor(a, "global")
	if c.purposeUA(a, "chat") != "explicit/1" || !strings.Contains(c.purposeUA(a, "refresh"), "6.0.0") || strings.Contains(c.purposeUA(nil, "chat"), "6.0.0") {
		t.Fatal("override leaked across realm/purpose")
	}
}
func TestV2RefreshSnapshotRealm(t *testing.T) {
	a := &auth.Auth{UID: "u", AccessToken: "old", RefreshToken: "r"}
	_, _ = auth.BackfillRealmFor(a, "global")
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "global.invalid" || !strings.Contains(r.UserAgent(), "WorkBuddy AI/5.6.2") || r.Header.Get("Origin") != "https://www.workbuddy.ai" {
			t.Fatal(r.URL, r.Header)
		}
		// refresh 对齐官方客户端：X-Auth-Refresh-Source=plugin；X-Domain 空 domain 回落端点 host。
		if r.Header.Get("X-Auth-Refresh-Source") != "plugin" {
			t.Fatalf("X-Auth-Refresh-Source=%q want plugin", r.Header.Get("X-Auth-Refresh-Source"))
		}
		if r.Header.Get("X-Domain") != "global.invalid" {
			t.Fatalf("X-Domain=%q want global.invalid (host fallback)", r.Header.Get("X-Domain"))
		}
		return jsonResp(200, `{"code":0,"data":{"accessToken":"new","refreshToken":"new-r","expiresIn":3600}}`), nil
	})
	c.GlobalEnabled = true
	c.ChatBaseGlobal = "https://global.invalid"
	if err := c.RefreshToken(a); err != nil {
		t.Fatal(err)
	}
}

// TestV2RefreshDomainFromAuth 账号落盘 domain 非空时，refresh 的 X-Domain 优先取该 domain（非 host）。
func TestV2RefreshDomainFromAuth(t *testing.T) {
	a := &auth.Auth{UID: "u", AccessToken: "old", RefreshToken: "r", Domain: "www.codebuddy.cn"}
	_, _ = auth.BackfillRealmFor(a, "cn")
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-Domain") != "www.codebuddy.cn" {
			t.Fatalf("X-Domain=%q want www.codebuddy.cn (from auth.domain)", r.Header.Get("X-Domain"))
		}
		return jsonResp(200, `{"code":0,"data":{"accessToken":"new","refreshToken":"new-r","expiresIn":3600}}`), nil
	})
	if err := c.RefreshToken(a); err != nil {
		t.Fatal(err)
	}
}

// TestV2RefreshHeaderFamily 校验 refresh 请求与官方客户端的头族逐一对应。
// 客户端的 refreshSession 只显式设 X-Refresh-Token / X-Auth-Refresh-Source / X-Domain，
// 其余靠全局 auth 拦截器注入：Authorization(Bearer 旧 accessToken)、X-User-Id、
// X-Enterprise-Id、X-Tenant-Id。该拦截器只要没设 X-No-Authorization 就会带 Authorization
// ——refresh 请求不设任何 X-No-*，所以这几头在真实客户端上全部存在。
func TestV2RefreshHeaderFamily(t *testing.T) {
	a := &auth.Auth{UID: "u-1", AccessToken: "old-at", RefreshToken: "rt-1", EnterpriseID: "ent-9", Domain: "www.codebuddy.cn"}
	_, _ = auth.BackfillRealmFor(a, "cn")
	var got http.Header
	c := testClient(func(r *http.Request) (*http.Response, error) {
		got = r.Header.Clone()
		return jsonResp(200, `{"code":0,"data":{"accessToken":"new","refreshToken":"new-r","expiresIn":3600}}`), nil
	})
	if err := c.RefreshToken(a); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"Authorization":         "Bearer old-at",
		"X-Refresh-Token":       "rt-1",
		"X-Auth-Refresh-Source": "plugin",
		"X-User-Id":             "u-1",
		"X-Enterprise-Id":       "ent-9",
		"X-Tenant-Id":           "ent-9",
		"X-Domain":              "www.codebuddy.cn",
	}
	for k, v := range want {
		if got.Get(k) != v {
			t.Errorf("%s=%q want %q", k, got.Get(k), v)
		}
	}
}

// TestV2DefaultUA 包级 DefaultUA（cmd/login 复用）与网关默认 purposeUA 同形：同 realm
// 下两者输出必须逐字相等，证明登录请求与出站请求使用同一 UA 逻辑。
func TestV2DefaultUA(t *testing.T) {
	c := New()
	for _, realm := range []string{"cn", "global"} {
		a := &auth.Auth{}
		_, _ = auth.BackfillRealmFor(a, realm)
		if got, want := DefaultUA(realm, "chat"), c.purposeUA(a, "chat"); got != want {
			t.Fatalf("realm=%s DefaultUA=%q want %q (= purposeUA default)", realm, got, want)
		}
	}
	if DefaultUA("cn", "chat") != "WorkBuddy/5.7.6 WorkBuddy/5.7.6 CLI/2.156.0" {
		t.Fatalf("cn DefaultUA=%q", DefaultUA("cn", "chat"))
	}
	if DefaultUA("global", "chat") != "WorkBuddy/5.6.2 WorkBuddy AI/5.6.2 CLI/2.147.0" {
		t.Fatalf("global DefaultUA=%q", DefaultUA("global", "chat"))
	}
}
func TestV2ConversationHeaders(t *testing.T) {
	c := New()
	r, _ := http.NewRequest("POST", "https://x.invalid", nil)
	a := &auth.Auth{UID: "u"}
	c.ChatHeaders(r, a, "", ChatMeta{ConversationID: "conv", ConversationRequestID: "turn", RootRequestID: "root", ParentConversationID: "parent", AgentType: "subagent", Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"})
	for k, v := range map[string]string{"X-Conversation-ID": "conv", "X-Conversation-Request-ID": "turn", "X-Root-Request-ID": "root", "X-Parent-Conversation-ID": "parent", "X-Agent-Type": "subagent", "X-B3-TraceId": "0123456789abcdef0123456789abcdef"} {
		if r.Header.Get(k) != v {
			t.Fatal(k, r.Header)
		}
	}
}
