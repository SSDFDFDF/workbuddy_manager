package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy_manager/internal/auth"
	"workbuddy_manager/internal/pool"
	"workbuddy_manager/internal/reqlog"
	"workbuddy_manager/internal/upstream"
)

// 本地默认思考档覆盖（upstream.effort_default_overrides）在 server 侧的生效面：
// 守护**真正发给上游的字节**——覆盖未配置时一个字段都不许多，配置命中时按该模型
// 的支持集落地（注入值必然可执行），越界请求回退到配置的默认档。
//
// /v1/models 宣告面的同源保证由 upstream 包的 EffortListing 测试覆盖；这里只测
// 「配置 → 出站请求」这一段的接线，因为漏接线的表现是「面板配了但请求不带档位」
// 这类静默不生效（与 session_sticky.enabled 曾经那类 bug 同形）。
func TestOutboundEffortDefaultInjection(t *testing.T) {
	// cn:glm-5.3-flash 的 CN 静态档位集 = [low high max]（默认档 high）。
	upstream.SetEffortDefaultOverrides(map[string]string{"glm-5.3-flash": "low"})
	t.Cleanup(upstream.ResetLookupChainForTest)

	for _, tc := range []struct {
		name string
		body string
		// wantEffort 空串 = 出站不得出现 reasoning_effort。
		wantEffort string
		// wantThinkingOff 出站 thinking.type 必须是 disabled（off 语义）。
		wantThinkingOff bool
	}{
		{"客户端未指定 → 注入本地默认档",
			`{"model":"cn:glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`, "low", false},
		{"客户端显式指定受支持档位 → 不动",
			`{"model":"cn:glm-5.3-flash","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`, "high", false},
		// 越界回退目标 = 本地默认档（不是「≤请求档位的最高支持档」= max）。
		{"客户端指定越界档位 → 回退本地默认档",
			`{"model":"cn:glm-5.3-flash","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"xhigh"}`, "low", false},
		{"reasoning.effort 形态越界 → 回退本地默认档",
			`{"model":"cn:glm-5.3-flash","messages":[{"role":"user","content":"hi"}],"reasoning":{"effort":"xhigh","summary":"auto"}}`, "low", false},
		// off/none 是「关掉思考」：既不能被默认档打开，也不参与回退。
		{"off → 不注入（保持关闭思考）",
			`{"model":"cn:glm-5.3-flash","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"off"}`, "", true},
		// thinking.type=disabled 是另一种「关思考」的客户端表态：注入会与它冲突。
		{"thinking.type=disabled → 不注入",
			`{"model":"cn:glm-5.3-flash","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cap := &outboundCapture{}
			h := captureHandler(t, Config{
				Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
				Upstream: &upstream.Client{},
			}, cap)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(tc.body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}
			var sent map[string]any
			if err := json.Unmarshal([]byte(cap.body), &sent); err != nil {
				t.Fatalf("出站不是合法 JSON: %v", err)
			}
			if got, _ := sent["reasoning_effort"].(string); got != tc.wantEffort {
				t.Fatalf("reasoning_effort=%q want %q (out=%s)", got, tc.wantEffort, cap.body)
			}
			th, _ := sent["thinking"].(map[string]any)
			if got, _ := th["type"].(string); (got == "disabled") != tc.wantThinkingOff {
				t.Fatalf("thinking.type=%q want disabled=%v (out=%s)", got, tc.wantThinkingOff, cap.body)
			}
			// 归一/注入都改写了文档：不得出现两个档位别名共存（forwarding 会判冲突 400）。
			if _, has := sent["reasoningEffort"]; has {
				t.Fatalf("camel 别名残留: %s", cap.body)
			}
		})
	}
}

// 未配置覆盖时**一个字段都不许多**：这是「本地的覆盖上游配置」的反面保证——
// 老配置（无该键）的行为必须与引入该能力之前逐字一致。
func TestOutboundEffortDefaultNoConfigNoInjection(t *testing.T) {
	upstream.ResetLookupChainForTest()
	t.Cleanup(upstream.ResetLookupChainForTest)

	cap := &outboundCapture{}
	h := captureHandler(t, Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: &upstream.Client{},
	}, cap)
	body := `{"model":"cn:glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(cap.body), &sent); err != nil {
		t.Fatalf("出站不是合法 JSON: %v", err)
	}
	if _, has := sent["reasoning_effort"]; has {
		t.Fatalf("未配置覆盖时不应注入档位: %s", cap.body)
	}
	if _, has := sent["thinking"]; has {
		t.Fatalf("未配置覆盖时不应改动 thinking: %s", cap.body)
	}
}

// 注入与越界回退都是**显式可见**的近似变换：归档 rewritten 列必须能回答
// 「我这次到底以什么档位执行的」（与越界归一同一通道，见 FORK_CHANGES §15）。
func TestOutboundEffortDefaultRecordedAsRewrite(t *testing.T) {
	upstream.SetEffortDefaultOverrides(map[string]string{"glm-5.3-flash": "low"})
	t.Cleanup(upstream.ResetLookupChainForTest)

	logs := reqlog.New(reqlog.Config{})
	cap := &outboundCapture{}
	h := captureHandler(t, Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   &upstream.Client{},
		RequestLog: logs,
	}, cap)
	body := `{"model":"cn:glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	s := logs.Snapshot()
	if len(s.Recent) != 1 {
		t.Fatalf("events=%d", len(s.Recent))
	}
	want := "reasoning.effort:默认→low"
	found := false
	for _, rw := range s.Recent[0].Rewritten {
		if rw == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("rewritten=%v want 含 %q", s.Recent[0].Rewritten, want)
	}
}

// 覆盖表按「实际选中账号所属域」解析：同名模型在 CN 与 global 的档位能力可以不同，
// 因此 "cn:model" 不得泄漏到 global 请求（反之亦然），而域通配/全局通配只作用于该域。
// 这一层放在 handler 的 attempt 循环里（与越界归一同一位置），因为它必须看到
// 轮转后真正被选中的那个账号的 realm。
func TestOutboundEffortDefaultIsRealmScoped(t *testing.T) {
	t.Cleanup(upstream.ResetLookupChainForTest)
	globalPool := func() *pool.Pool {
		return testPoolWith(&auth.Auth{
			UID: "g1", AccessToken: "at-g", ExpiresAt: 9999999999,
			Domain: "www.workbuddy.ai", // → Realm()=="global"
		})
	}
	cnPool := func() *pool.Pool {
		return testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	}

	for _, tc := range []struct {
		name       string
		overrides  map[string]string
		pool       *pool.Pool
		model      string
		wantEffort string
	}{
		// 域前缀：只作用于声明的那一域。glm-5.3-flash 在 global 面没有静态兜底，
		// CN 面的条目泄漏过去只会变成一声必 400 的非法参数。
		{"cn 专属条目不作用于 global 请求", map[string]string{"cn:glm-5.3-flash": "low"}, globalPool(), "global:glm-5.3-flash", ""},
		{"global 专属条目作用于 global 请求", map[string]string{"global:glm-5.3-flash": "low"}, globalPool(), "global:glm-5.3-flash", "low"},
		{"global 通配作用于 global 请求", map[string]string{"global:*": "low"}, globalPool(), "global:glm-5.3", "low"},
		// 全局通配：两边都生效（但依然要求档位能力已知）。
		{"全局通配作用于 global 请求", map[string]string{"*": "xhigh"}, globalPool(), "global:glm-5.2", "xhigh"},
		// CN 域条目对 CN 账号同样生效（回归：域前缀不得把裸名条目也挡掉）。
		{"裸名条目作用于 CN 请求", map[string]string{"glm-5.3-flash": "low"}, cnPool(), "cn:glm-5.3-flash", "low"},
		// 反向隔离：global 专属条目不得作用于 CN 请求。
		{"global 专属条目不作用于 CN 请求", map[string]string{"global:glm-5.3-flash": "low"}, cnPool(), "cn:glm-5.3-flash", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream.SetEffortDefaultOverrides(tc.overrides)
			cap := &outboundCapture{}
			h := captureHandler(t, Config{
				Pool:          tc.pool,
				Upstream:      &upstream.Client{GlobalEnabled: true},
				GlobalEnabled: true,
			}, cap)
			body := `{"model":"` + tc.model + `","messages":[{"role":"user","content":"hi"}]}`
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}
			var sent map[string]any
			if err := json.Unmarshal([]byte(cap.body), &sent); err != nil {
				t.Fatalf("出站不是合法 JSON: %v", err)
			}
			if got, _ := sent["reasoning_effort"].(string); got != tc.wantEffort {
				t.Fatalf("reasoning_effort=%q want %q (out=%s)", got, tc.wantEffort, cap.body)
			}
		})
	}
}

// /v1/models 宣告面：本地覆盖必须落到 reasoning_default_effort 上，且凡是宣告
// 出去的值都必须是该模型的支持档（客户端照宣告选档是本网关自己的硬前提）。
// 上游探测下发的 defaultEffort（dyn-model-a = medium）在这一层被本地值压过。
func TestModelsDeclaresLocalDefaultOverride(t *testing.T) {
	t.Cleanup(upstream.ResetLookupChainForTest)

	newHandler := func(t *testing.T) *Handler {
		t.Helper()
		up := newFakeUpstream(t, func(string) (int, string, bool) {
			return 200, `{"code":0,"data":{"models":[{"id":"dyn-model-a","maxInputTokens":65536,"maxOutputTokens":8192,"reasoning":{"effort":"medium","supportedEfforts":["low","medium","high"]}}],"agents":[{"name":"cli","models":["dyn-model-a"]}]}}`, false
		})
		p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
		h := NewHandler(Config{Pool: p, Upstream: up})
		if _, err := up.RefreshCatalog(p.AuthByUID("u1")); err != nil {
			t.Fatalf("RefreshCatalog: %v", err)
		}
		return h
	}

	declaredDefault := func(t *testing.T, h *Handler) string {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("list models: %v", err)
		}
		for _, m := range resp["data"].([]any) {
			mm := m.(map[string]any)
			if mm["id"] == "cn:dyn-model-a" {
				def, _ := mm["reasoning_default_effort"].(string)
				// 宣告的默认档必须在宣告的支持集里（三处同源的不可缺一环）。
				if def != "" {
					supported, _ := mm["reasoning_supported_efforts"].([]any)
					ok := false
					for _, s := range supported {
						if s == def {
							ok = true
						}
					}
					if !ok {
						t.Fatalf("宣告的默认档 %q 不在支持集 %v 里", def, supported)
					}
				}
				return def
			}
		}
		t.Fatal("cn:dyn-model-a 不在列表里")
		return ""
	}

	// 基线：无覆盖 → 沿用远端下发的默认档。
	if got := declaredDefault(t, newHandler(t)); got != "medium" {
		t.Fatalf("基线默认档=%q want medium", got)
	}
	// 覆盖命中（裸名 / 域前缀都作用于 CN）。
	for _, ov := range []map[string]string{{"dyn-model-a": "high"}, {"cn:dyn-model-a": "high"}, {"cn:*": "high"}} {
		upstream.SetEffortDefaultOverrides(ov)
		if got := declaredDefault(t, newHandler(t)); got != "high" {
			t.Fatalf("覆盖 %v 后宣告默认档=%q want high", ov, got)
		}
	}
	// 覆盖值超出该模型支持集 → 按档位序钳位（这里 high 是 ≤xhigh 的最高支持档），
	// 绝不要把越界值宣告出去（那等于叫客户端选一个必 400 的档位）。
	upstream.SetEffortDefaultOverrides(map[string]string{"dyn-model-a": "xhigh"})
	if got := declaredDefault(t, newHandler(t)); got != "high" {
		t.Fatalf("越界覆盖应被钳到 high，实际 %q", got)
	}
	// 另一域专属条目不作用于 CN 请求。
	upstream.SetEffortDefaultOverrides(map[string]string{"global:dyn-model-a": "low"})
	if got := declaredDefault(t, newHandler(t)); got != "medium" {
		t.Fatalf("global 条目泄漏到 CN：%q", got)
	}
}

// Responses 桥要回显**实际生效**的档位：注入发生在请求侧，客户端在响应里能看见
// 自己没指定却被执行的档位（与越界归一回显同一契约）。
func TestResponsesEchoesInjectedDefaultEffort(t *testing.T) {
	upstream.SetEffortDefaultOverrides(map[string]string{"glm-5.3-flash": "low"})
	t.Cleanup(upstream.ResetLookupChainForTest)

	cap := &outboundCapture{}
	h := captureHandler(t, Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: &upstream.Client{},
	}, cap)
	body := `{"model":"cn:glm-5.3-flash","store":false,"stream":false,"input":"hi"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (%s)", err, rec.Body.String())
	}
	reasoning, _ := resp["reasoning"].(map[string]any)
	if got, _ := reasoning["effort"].(string); got != "low" {
		t.Fatalf("响应未回显生效档位: reasoning=%v (body=%s)", resp["reasoning"], rec.Body.String())
	}
}
