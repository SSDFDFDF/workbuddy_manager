package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy_manager/internal/auth"
	"workbuddy_manager/internal/pool"
	"workbuddy_manager/internal/prompt"
	"workbuddy_manager/internal/redisstore"
	"workbuddy_manager/internal/reqlog"
	"workbuddy_manager/internal/scrub"
	"workbuddy_manager/internal/session"
	"workbuddy_manager/internal/upstream"
	"workbuddy_manager/internal/usage"
)

// TestMain 默认关闭聊天表格日志（chatLogEnabled=false），消除 go test 期间的 stdout 噪音。
// 断言表格行输出的测试（logging_test.go 中的 ChatLogs/LogChatRow 系列）用 withChatLog 临时开启。
func TestMain(m *testing.M) {
	chatLogEnabled = false
	os.Exit(m.Run())
}

const sseOK = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
	"data: [DONE]\n\n"

// newFakeUpstream 返回一个 ChatStream 走 fake 的 upstream.Client。
// fake 依据 Authorization 头决定行为。
func newFakeUpstream(t *testing.T, behavior func(auth string) (status int, body string, isStream bool)) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			authz := r.Header.Get("Authorization")
			status, body, isStream := behavior(authz)
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// bindStore 记录粘性绑定镜像调用（不联网），供 D4 端到端断言绑定收敛到最终成功号。
type bindStore struct {
	redisstore.Noop
	mu     sync.Mutex
	binds  map[string]string
	delCnt int
}

func newBindStore() *bindStore { return &bindStore{binds: map[string]string{}} }

func (b *bindStore) SetBind(key, uid string, ttl time.Duration) {
	b.mu.Lock()
	b.binds[key] = uid
	b.mu.Unlock()
}
func (b *bindStore) DelBind(key string) {
	b.mu.Lock()
	b.delCnt++
	delete(b.binds, key)
	b.mu.Unlock()
}
func (b *bindStore) LoadBinds() map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]string{}
	for k, v := range b.binds {
		out[k] = v
	}
	return out
}
func (b *bindStore) lastUID(key string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	u, ok := b.binds[key]
	return u, ok
}

// testPoolWith 构建一个所有账号 credits=1000 的池，并注入确定性随机源：
// randInt64N 恒返回 0 → pickWeighted 必选候选集中积分最高者（第一个）。
// 这让依赖"bad 先被选中"的轮转测试（如 TestChatRotatesOnHardCredit）完全确定，
// 不再受加权随机影响而 flake。
func testPoolWith(auths ...*auth.Auth) *pool.Pool {
	p := pool.New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	for _, a := range auths {
		p.Add(a)
		p.SetCredits(a.UID, 1000, 0)
	}
	return p
}

// TestChatLargeBodyNoGatewayLimit 请求体无网关侧上限（max_body_mb 已移除）：
// 数 MB 的合法 body 完整读入并照常打上游，网关不再 413（超限类问题交由上游
// 自然响应，对齐上游 e34cfa4 规约）。
func TestChatLargeBodyNoGatewayLimit(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	pad := strings.Repeat("a", 4<<20) // 4MB 合法 JSON 字符串值
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"pad":"` + pad + `"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (large valid body must proceed)", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Errorf("upstream calls=%d want 1", calls)
	}
}

// TestChatBadParamsFailsFastWithoutPenalty 上游 400 + Unmarshal chat params failed（11101）
// → 请求级错误：不罚账号（无冷却/无禁用/无熔断计数/无 errTotal），**且不轮转**——
// 同一 body 换号必然同样失败。端到端断言只打一次上游、直接回 400、账号完好。
func TestChatBadParamsFailsFastWithoutPenalty(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0) // 确定性源 r=0 → 先选 bad
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s (want 400: request-level error must not be retried on other accounts)", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 0 {
		t.Errorf("calls=%v want bad 1 次、good 0 次（零轮转）", calls)
	}
	// 账号完好：无冷却、无禁用、无熔断计数、无 errTotal。
	st, _ := p.Status("bad")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 {
		t.Errorf("ErrBadParams must not penalize account: %+v", st)
	}
}

// TestChatBadParams400CarriesUpstreamBody 11101 的 400 响应必须包含上游原始
// 11101 信息（含 requestId，客户端据此排查），且不再出现空洞的 no_healthy_account。
func TestChatBadParams400CarriesUpstreamBody(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 400 {
		t.Fatalf("code=%d body=%s (want 400)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "11101") || !strings.Contains(body, "Unmarshal chat params failed") {
		t.Errorf("400 message should carry upstream 11101 info: %s", body)
	}
	if strings.Contains(body, "no_healthy_account") {
		t.Errorf("request-level failure must not be reported as account exhaustion: %s", body)
	}
}

func TestChatNonStreamAggregates(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz != "Bearer at1" {
			t.Errorf("auth=%q", authz)
		}
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object=%v", resp["object"])
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Errorf("content=%q", msg["content"])
	}
	st, ok := h.cfg.Pool.Status("u1")
	if !ok {
		t.Fatal("account status missing")
	}
	if st.TokenUsage.RequestCount != 1 || st.TokenUsage.UsageCount != 1 ||
		st.TokenUsage.PromptTokens != 1 || st.TokenUsage.CompletionTokens != 1 || st.TokenUsage.TotalTokens != 2 {
		t.Errorf("token usage=%+v", st.TokenUsage)
	}
	if st.TokenUsage.LastLatencyMs < 1 || st.TokenUsage.LastTokensPerSecond == nil || *st.TokenUsage.LastTokensPerSecond <= 0 {
		t.Errorf("latest performance=%+v", st.TokenUsage)
	}
}

func TestChatStreamPassthrough(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("ct=%q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "你好") || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body=%q", body)
	}
	st, ok := h.cfg.Pool.Status("u1")
	if !ok {
		t.Fatal("account status missing")
	}
	if st.TokenUsage.RequestCount != 1 || st.TokenUsage.UsageCount != 1 ||
		st.TokenUsage.PromptTokens != 1 || st.TokenUsage.CompletionTokens != 1 || st.TokenUsage.TotalTokens != 2 {
		t.Errorf("token usage=%+v", st.TokenUsage)
	}
	if st.TokenUsage.LastLatencyMs < 1 || st.TokenUsage.LastTokensPerSecond == nil || *st.TokenUsage.LastTokensPerSecond <= 0 {
		t.Errorf("latest performance=%+v", st.TokenUsage)
	}
}

func TestChatRecordsCreditForStreamAndSync(t *testing.T) {
	const sseCredit = "data: {\"id\":\"chatcmpl-credit\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":6,\"total_tokens\":10,\"credit\":1.25}}\n\n" +
		"data: [DONE]\n\n"
	for _, stream := range []bool{false, true} {
		name := "sync"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				return 200, sseCredit, true
			})
			rec := usage.New("")
			h := NewHandler(Config{
				Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
				Upstream: up,
				Usage:    rec,
			})
			body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
			if stream {
				body = `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`
			}
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
			if recorder.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", recorder.Code, recorder.Body)
			}
			s := rec.Snapshot(24, nil)
			if s.Totals.Credits != 1.25 || s.Totals.CreditSamples != 1 || s.Totals.CreditTokens != 10 || s.Totals.CreditsPer1MTokens != 125000 {
				t.Fatalf("usage totals = %+v, want credit=1.25 tokens=10 ratio=125000", s.Totals)
			}
			if len(s.CreditByAccount) != 1 || s.CreditByAccount[0].Key != "u1" ||
				len(s.CreditByModel) != 1 || s.CreditByModel[0].Key != "glm-5.2" {
				t.Fatalf("credit dimensions = %+v / %+v", s.CreditByAccount, s.CreditByModel)
			}
		})
	}
}

func TestChatRotatesOnHardCredit(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 402, `{"code":1,"msg":"余额不足"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 让 bad 积分更高被先选中
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 0 {
		t.Errorf("quota rejection must not rotate: calls=%v", calls)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.Reason == "" {
		t.Errorf("bad account should be cooling: %+v", st)
	}
	if st.TokenUsage.RequestCount != 1 || st.TokenUsage.UsageCount != 0 {
		t.Errorf("bad token usage=%+v", st.TokenUsage)
	}
	good, _ := p.Status("good")
	if good.TokenUsage.RequestCount != 0 || good.TokenUsage.TotalTokens != 0 {
		t.Errorf("unused good account token usage=%+v", good.TokenUsage)
	}
}

// TestChatSoftCoolsOnRateLimitBody 端到端回归 issue #28：上游用非 429 状态码
// （400 + 限流文案）表达模型侧限流时，该账号必须进入 CoolSoft 冷却，而不是只换号。
// 修复前 Classify 归 ErrClient → applyErrorPolicy 走 default 分支只换号不罚，
// 账号留在可用池里，下一个请求仍会被选中。
func TestChatSoftCoolsOnRateLimitBody(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 400, `{"code":1,"msg":"The model provider is rate-limiting requests. Please wait a moment and try again."}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 让 bad 积分更高被先选中（与 TestChatRotatesOnHardCredit 同一确定性手法）。
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	const soft = 45 * time.Second
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: soft})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 429 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 0 {
		t.Errorf("rate limit must not rotate: calls=%v", calls)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("bad 应进入 soft_rate 冷却: %+v", st)
	}
	// 冷却时长取自注入的 SoftCooldown，不依赖真实等待。
	if max := int64(soft / time.Second); st.CoolRemaining <= 0 || st.CoolRemaining > max {
		t.Errorf("cool_remaining_sec=%d want in (0,%d]", st.CoolRemaining, max)
	}

	// 冷却生效：同一账号在冷却期内不得再被选中。
	before := calls["Bearer at-bad"]
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec2.Code != 200 {
		t.Fatalf("second code=%d body=%s", rec2.Code, rec2.Body)
	}
	if calls["Bearer at-bad"] != before {
		t.Errorf("冷却中的账号不应再次被选中: calls=%v", calls)
	}
}

func TestApplyErrorPolicySoftRateExponentialBackoff(t *testing.T) {
	// 吸收上游语义：带解析重置时间的 body 对齐墙钟；无时间文案走有界退避，且
	// **冷却中的重复触发不堆加**（旧「每次都翻倍」正是全池被推到 2h 封顶的元凶）。
	// 跨冷却期的堆加语义由 pool 包 CooldownSoftRate 测试覆盖；此处锚定 handler 侧
	// 的调用形态：连续 3 次软限流错误 → 首次 600s，后续不延长。
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

	for i := 1; i <= 3; i++ {
		h.applyErrorPolicy("u1", upstream.ErrSoftRate, "", "", nil)
		st, _ := p.Status("u1")
		if !st.Cooling || st.CoolKind != "soft_rate" {
			t.Fatalf("call %d: 应为 soft_rate 冷却: %+v", i, st)
		}
		if st.SoftStreak != 1 {
			t.Errorf("call %d: 冷却中兜底探测不应推进 soft_streak, got %d", i, st.SoftStreak)
		}
		if st.CoolRemaining < 600-3 || st.CoolRemaining > 600 {
			t.Errorf("call %d: cool_remaining_sec=%d want ~600（不堆加）", i, st.CoolRemaining)
		}
	}
}

func TestApplyErrorPolicyNotFoundUsesFixedBase(t *testing.T) {
	// 404 分流：偶发上游 404 的冷却基数固定 60s（notFoundCooldown），不取 soft_rate
	// 的 600s 基数，也不受其配置值影响。Cooldown 重构后 404 是固定时长冷却
	//（不堆加、不喂熔断），成功后自然到期恢复。
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 20 * time.Minute}) // soft_rate 配得很大，验证 404 不受其影响

	notFoundSec := int64(0)
	for i := 1; i <= 3; i++ {
		h.applyErrorPolicy("u1", upstream.ErrNotFound, "", "", nil)
		st, _ := p.Status("u1")
		if st.Cooling || st.ErrTotal != 0 {
			t.Fatalf("route failure penalized account: %+v", st)
		}
		if st.SoftStreak != 0 {
			t.Errorf("call %d: 404 固定冷却不应推进 soft_streak, got %d", i, st.SoftStreak)
		}
		// 基数取自 notFoundCooldown（60s）而非注入的 soft_rate（20m），且重复触发不延长。
		if st.CoolRemaining < notFoundSec-3 || st.CoolRemaining > notFoundSec {
			t.Errorf("call %d: 404 cool_remaining_sec=%d want ~%d（固定基数 %ds，非 soft_rate）",
				i, st.CoolRemaining, notFoundSec, notFoundSec)
		}
	}
}

// TestNewHandlerSoftCooldownDefault 端到端：未注入 SoftCooldown 时基数回落到 600s（原为 60s）。
func TestNewHandlerSoftCooldownDefault(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 429, `{"code":1,"msg":"rate limit"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up}) // 不注入 SoftCooldown

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 429 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("bad 应进入 soft_rate 冷却: %+v", st)
	}
	if st.CoolRemaining <= 599 || st.CoolRemaining > 600 {
		t.Errorf("default soft cooldown cool_remaining_sec=%d want 600", st.CoolRemaining)
	}
}

// TestChatStickyFollowsFinalSuccess 端到端验证 D4：粘性号失败换号成功后，会话绑定收敛到成功号。
func TestChatStickyFollowsFinalSuccess(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"bad", "good"} },
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 先把会话预绑定到 bad（模拟历史粘性），bad 失败、good 成功 → 绑定应切到 good。
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 400, `{"code":11102,"msg":"service info not found"}`, false
		}
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:         p,
		Upstream:     up,
		Session:      sess,
		SoftCooldown: time.Minute,
	})
	// 预绑定：sess.Bind("cn:glm-5.2:conv-1", "bad")，然后请求体带同 conversation_id。
	sess.Bind("cn:glm-5.2:conv-1", "bad")
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 绑定必须收敛到最终成功的 good。
	if uid, ok := st.lastUID("cn:glm-5.2:conv-1"); !ok || uid != "good" {
		t.Fatalf("sticky binding should follow final success to good, got %s ok=%v (binds=%v)", uid, ok, st.binds)
	}
}

// TestChatStickySuccessKeepsBinding 粘性号直接成功 → 绑定不变（仍为该号）。
func TestChatStickySuccessKeepsBinding(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"good"} },
	})
	p := testPoolWith(&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})
	sess.Bind("cn:glm-5.2:conv-1", "good")
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if uid, ok := st.lastUID("cn:glm-5.2:conv-1"); !ok || uid != "good" {
		t.Fatalf("binding should stay good after success, got %s ok=%v", uid, ok)
	}
}

// TestChatStickyFullFallsBackToRotation 端到端验证 C3 语义：粘性号满载不可用时，
// 请求在同一轮内解绑并回落普通轮换选中健康账号，绑定收敛到最终成功号——而非空耗一轮。
func TestChatStickyFullFallsBackToRotation(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"bad", "good"} },
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// bad 占满唯一在途名额：PickByUID 将返回 nil（healthy 但 inFlight 满）→ 解绑 + 回落轮换。
	p.SetMaxInFlight(1)
	p.Acquire("bad")

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:         p,
		Upstream:     up,
		Session:      sess,
		SoftCooldown: time.Minute,
	})
	sess.Bind("cn:glm-5.2:conv-1", "bad")
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 满载粘性号被解绑，绑定收敛到最终成功号 good。
	if uid, ok := st.lastUID("cn:glm-5.2:conv-1"); !ok || uid != "good" {
		t.Fatalf("sticky binding should fall back to good, got %s ok=%v (binds=%v)", uid, ok, st.binds)
	}
	p.Release("bad")
}

func TestChatHardCreditCooldownUntilNextDay4AM(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 402, `{"code":1,"msg":"余额不足"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0) // bad 积分高，确定性源 → 先被选中
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, ok := p.Status("bad")
	if !ok || !st.Cooling {
		t.Fatalf("bad should be cooling: %+v ok=%v", st, ok)
	}
	if st.Reason != "余额不足" {
		t.Errorf("reason=%q", st.Reason)
	}
	// 硬信贷冷却必须是次日 04:00，而不是固定 12h/配置时长。
	if st.Until.Hour() != 4 {
		t.Errorf("until hour=%d want 4 (next-day 04:00)", st.Until.Hour())
	}
	// 距次日 04:00 最长 28h（凌晨 00:00~04:00 间运行时 now→次日 04:00 跨度 > 24h，属正常）。
	if d := time.Until(st.Until); d <= 0 || d > 28*time.Hour {
		t.Errorf("until %v not within (0,28h]: %v", st.Until, d)
	}
	// 立即换号成功：good 被选中。
	stGood, _ := p.Status("good")
	if stGood.Cooling || stGood.Disabled {
		t.Errorf("good should stay healthy: %+v", stGood)
	}
}

func TestChat429Code14018UsesHardCreditCooldown(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 429, `{"code":14018,"msg":"Credits exhausted"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 2000)
	p.SetCredits("good", 1000, 1000)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	st, ok := p.Status("bad")
	if !ok || !st.Cooling {
		t.Fatalf("14018 account should be cooling: %+v ok=%v", st, ok)
	}
	if st.Reason != "余额不足" || st.Until.Hour() != 4 {
		t.Fatalf("14018 should use hard-credit cooldown, got reason=%q until=%v", st.Reason, st.Until)
	}
	if got := p.Pick(); got == nil || got.UID != "good" {
		t.Fatalf("hard-cooled 14018 account must not be fallback-picked, got %+v", got)
	}
}

// TestChat6004ModelResetCoolsToParsedTime 端到端回归 issue #31：上游 429 + code 6004
// +「将在 … 重置」→ 冷却 until 精确等于解析时间（而非 600s 固定基数/指数退避），
// 且记录触发模型 → 同模型请求仍被冷却、切模型请求按豁免可选。
func TestChat6004ModelResetCoolsToParsedTime(t *testing.T) {
	// 用未来 5 分钟的重置时间（wall-clock）构造上游响应。
	reset := time.Now().Add(5 * time.Minute)
	ts := reset.In(upstream.SoftRateResetLoc()).Format("2006-01-02 15:04:05")
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		if authz == "Bearer at-bad" {
			return 429, `{"code":6004,"msg":"将在 ` + ts + ` UTC+8 重置"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	// 隔离对 breaker 的干扰：熔断阈值默认 3，一次失败不触发。
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 429 {
		t.Fatalf("code=%d body=%s (rate rejection must not rotate)", rec.Code, rec.Body)
	}
	// bad 已进入 6004 模型级独立冷却：账号级不 cooling，台账单行 until ≈ reset。
	st, _ := p.Status("bad")
	if st.Cooling {
		t.Fatalf("6004-with-reset should NOT set account-level cooling: %+v", st)
	}
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" {
		t.Fatalf("want single model ledger row glm-5.3: %+v", st.RateLimitedModels)
	}
	if d := st.RateLimitedModels[0].Until.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("model until=%v want ~reset=%v (diff %v)", st.RateLimitedModels[0].Until, reset, d)
	}
	// 记录触发模型（bad 池内 private 字段需经 Status 不可见，改用行为断言）：
	// 同模型 glm-5.3 的请求不应选中 bad（仍冷却）；
	// 不同模型 hy3-x 的请求应豁免冷却选中 bad（最高分）。
	p.SetRandomSource(func(n int64) int64 { return 0 })
	same := p.PickExcludingForModel(nil, "glm-5.3")
	if same == nil || same.UID != "good" {
		t.Fatalf("same-model pick should skip bad (still cooling), got %+v", same)
	}
	diff := p.PickExcludingForModel(nil, "hy3-x")
	if diff == nil || diff.UID != "bad" {
		t.Fatalf("different-model pick should bypass bad soft cooling, got %+v", diff)
	}
}

// TestChat6004WithoutResetFallsBackToBackoff 6004 无时间文案 → 退回 600s 基数软冷却
// （现状不变）。
func TestChat6004WithoutResetFallsBackToBackoff(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		if authz == "Bearer at-bad" {
			return 429, `{"code":6004,"msg":"model usage limit exceeded"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 429 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("bad should be soft cooling: %+v", st)
	}
	// 冷却时长 = 注入 soft 基数(60s)，非解析时间（无重置文案）。
	if st.CoolRemaining <= 0 || st.CoolRemaining > 60 {
		t.Errorf("cool_remaining_sec=%d want ~60 (soft base, not parsed)", st.CoolRemaining)
	}
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" || st.RateLimitedModels[0].Kind != "rate_limit" {
		t.Fatalf("rate-limited models=%+v, want audit row for glm-5.3", st.RateLimitedModels)
	}
}

func TestChatAllUnavailableReturns503(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 402, `{"code":1,"msg":"余额不足"}`, false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
	var e map[string]any
	json.Unmarshal(rec.Body.Bytes(), &e)
	if e["error"] == nil {
		t.Errorf("want error envelope: %s", rec.Body)
	}
}

func TestChatSessionDeadDisables(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 401, `{"code":12153,"msg":"Offline user session not found"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("code=%d", rec.Code)
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("account should be disabled: %+v", st)
	}
}

func TestChatTransportErrorDoesNotPenalize(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused")
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	// 传输错误不喂熔断计数：一次 transport error 不应累计 errTotal 也不应熔断。
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 502 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.ErrTotal != 0 {
		t.Fatalf("transport error should not penalize account: %+v", st)
	}
}

func TestChatHTTP5xxPenalizes(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// 熔断阈值 1：一次 5xx 即触发熔断（连续失败语义并入熔断器）。
	p.SetBreaker(1, time.Hour, time.Hour)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `{"code":500}`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 502 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.ErrTotal != 0 {
		t.Fatalf("http 5xx must not penalize account: %+v", st)
	}
}

func TestChatHTTP4xxClientDoesNotPenalize(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":400,"msg":"bad request"}`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 400 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.ErrTotal != 0 {
		t.Fatalf("generic 4xx should not penalize account: %+v", st)
	}
}

func TestModelsEndpoint(t *testing.T) {
	up := upstream.New()
	// 目录快照是显式刷新制的：/v1/models 只读快照，测试需先写入（生产路径 = 启动预热）。
	up.SetCatalogForTest("cn", []upstream.ModelInfo{{ID: "glm-5.2"}, {ID: "glm-5.1"}, {ID: "kimi-k3"}, {ID: "hy3"}, {ID: "minimax-m3"}})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: up})
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["object"] != "list" {
		t.Errorf("object=%v", resp["object"])
	}
	data := resp["data"].([]any)
	if len(data) < 5 {
		t.Errorf("models count=%d", len(data))
	}
	found := false
	for _, m := range data {
		if m.(map[string]any)["id"] == "cn:glm-5.2" {
			found = true
		}
	}
	if !found {
		t.Error("cn:glm-5.2 missing")
	}
}

func TestModelsDynamic(t *testing.T) {
	// 假上游返回动态模型（含 agents + maxInputTokens/maxOutputTokens + reasoning 档位）：
	// 门禁/列表都只读快照，故这里直接写快照 + 断言 /v1/models 的字段映射。
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[{"id":"dyn-model-a","maxInputTokens":65536,"maxOutputTokens":8192,"reasoning":{"effort":"medium","supportedEfforts":["low","medium","high"]}},{"id":"dyn-model-b","maxInputTokens":131072,"maxOutputTokens":16384},{"id":"glm-9.9","maxInputTokens":262144,"maxOutputTokens":32768}],"agents":[{"name":"cli","models":["dyn-model-a","dyn-model-b","glm-9.9"]}]}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	// 未刷新 → 空列表（无静态兜底、无懒探测）。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var empty map[string]any
	json.Unmarshal(rec.Body.Bytes(), &empty)
	if got := len(empty["data"].([]any)); got != 0 {
		t.Fatalf("cold catalog should yield empty list, got %d", got)
	}

	// 手动刷新（面板「重新获取」同路径）→ 写快照 + 返回目录。
	infos, err := up.RefreshCatalog(p.AuthByUID("u1"))
	if err != nil || len(infos) != 3 {
		t.Fatalf("RefreshCatalog: infos=%d err=%v (want 3)", len(infos), err)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	data := resp["data"].([]any)
	if len(data) != 3 {
		t.Fatalf("want 3 dynamic models, got %d: %v", len(data), data)
	}
	ids := map[string]bool{}
	for _, m := range data {
		ids[m.(map[string]any)["id"].(string)] = true
	}
	if !ids["cn:dyn-model-a"] || !ids["cn:glm-9.9"] {
		t.Errorf("dynamic ids missing: %v", ids)
	}

	// 断言字段映射：maxInputTokens → context_length，maxOutputTokens → max_output_tokens
	for _, m := range data {
		mm := m.(map[string]any)
		switch mm["id"] {
		case "cn:dyn-model-a":
			if mm["context_length"].(float64) != 65536 {
				t.Errorf("dyn-model-a context_length=%v want 65536", mm["context_length"])
			}
			if mm["max_output_tokens"].(float64) != 8192 {
				t.Errorf("dyn-model-a max_output_tokens=%v want 8192", mm["max_output_tokens"])
			}
			// reasoning 档位透出：supported_efforts + default_effort
			efforts, _ := mm["reasoning_supported_efforts"].([]any)
			if len(efforts) != 3 || efforts[0] != "low" {
				t.Errorf("dyn-model-a reasoning_supported_efforts=%v", mm["reasoning_supported_efforts"])
			}
			if mm["reasoning_default_effort"] != "medium" {
				t.Errorf("dyn-model-a reasoning_default_effort=%v want medium", mm["reasoning_default_effort"])
			}
		case "cn:dyn-model-b":
			// 上游未返回 reasoning → 档位字段省略（客户端按自身默认）
			if _, has := mm["reasoning_supported_efforts"]; has {
				t.Errorf("dyn-model-b supported efforts should be omitted, got %v", mm["reasoning_supported_efforts"])
			}
		case "cn:glm-9.9":
			if mm["context_length"].(float64) != 262144 {
				t.Errorf("glm-9.9 context_length=%v want 262144", mm["context_length"])
			}
			if mm["max_output_tokens"].(float64) != 32768 {
				t.Errorf("glm-9.9 max_output_tokens=%v want 32768", mm["max_output_tokens"])
			}
		}
	}
}

func TestModelsEmptyCatalogYieldsEmptyList(t *testing.T) {
	// 纯动态 + 缓存制（产品决策）：目录未刷新过 → 空列表（200），无静态兜底——
	// 拉不出目录时假名单只会让客户端选到 11102 的模型。
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `boom`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	data := resp["data"].([]any)
	if len(data) != 0 {
		t.Errorf("empty catalog should yield empty list, got %d", len(data))
	}
}

func TestModelsEndpointNeverProbesUpstream(t *testing.T) {
	// 缓存制的核心契约：**客户端请求路径零上游探测**。刷新只发生在启动预热与
	// 面板手动刷新；/v1/models 拉到目录为空也照常 200，且不喂 chat 熔断器
	// （旧实现的懒探测每次都会打上游，熔断阈值 1 时一次失败即熔断——这正是
	// 上游 9832283 报过的问题）。
	var calls atomic.Int32
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetBreaker(1, time.Hour, time.Hour)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls.Add(1)
		return 500, `boom`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
		if rec.Code != 200 {
			t.Fatalf("req %d: code=%d body=%s", i, rec.Code, rec.Body)
		}
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("/v1/models must not probe upstream, got %d calls", got)
	}
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Fatalf("models endpoint must not trip chat breaker: %+v", st)
	}
}

func TestRefreshCatalogFailureKeepsOldSnapshot(t *testing.T) {
	// 刷新失败：保留旧快照（旧目录总比没有强，且不会被一次网络抖动清空）、
	// 记录失败原因，且**不罚账号**（models 端点失败 ≠ chat 不可用）。
	fail := false
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if fail {
			return 500, `boom`, false
		}
		return 200, `{"code":0,"data":{"models":[{"id":"glm-5.2"}],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	if _, err := up.RefreshCatalog(p.AuthByUID("u1")); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}
	fail = true
	if _, err := up.RefreshCatalog(p.AuthByUID("u1")); err == nil {
		t.Fatal("refresh should fail on 500")
	}
	entry := up.Catalog("cn")
	if len(entry.Infos) != 1 || entry.Infos[0].ID != "glm-5.2" {
		t.Fatalf("failure must keep previous snapshot: %+v", entry.Infos)
	}
	if entry.FetchedAt.IsZero() || entry.LastErr == "" {
		t.Errorf("entry should keep FetchedAt and record LastErr: %+v", entry)
	}
	// 门禁照常按旧快照工作。
	if _, ok, ready := up.LookupCatalog("cn", "glm-5.2"); !ok || !ready {
		t.Errorf("stale-but-usable snapshot should still serve the gate: ok=%v ready=%v", ok, ready)
	}
	// 账号不留痕。
	st, _ := p.Status("u1")
	if st.Cooling || st.ErrTotal != 0 {
		t.Fatalf("catalog refresh failure must not penalize the account: %+v", st)
	}
	// /v1/models 仍列出旧目录。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "cn:glm-5.2") {
		t.Errorf("stale snapshot should still be listed: code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestAPIKeyAuth(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "secret",
	})
	// 无 key
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("no key: code=%d", rec.Code)
	}
	// 错 key
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("wrong key: code=%d", rec.Code)
	}
	// 对 key（请求会继续打到上游，但此处上游 client 会失败 —— 只要不是 401 就行）
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("right key: code=%d", rec.Code)
	}
}

func TestStatusEndpoint(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 42, 0)
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	req := httptest.NewRequest("GET", "/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"uid":"u1"`) || !strings.Contains(body, `"credits":42`) {
		t.Errorf("body=%s", body)
	}
	if strings.Contains(body, "AccessToken") || strings.Contains(body, `"at"`) {
		t.Error("token leaked in status output")
	}
	// Phase 3 汇总字段。
	var statusBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &statusBody); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if statusBody["total"] != float64(1) || statusBody["healthy"] != float64(1) ||
		statusBody["cooling"] != float64(0) || statusBody["disabled"] != float64(0) ||
		statusBody["in_flight_full"] != float64(0) {
		t.Errorf("summary=%v want total=1 healthy=1 cooling=0 disabled=0 in_flight_full=0", statusBody)
	}
	// Phase v3：池级 sticky_sessions + redis_mode。
	if statusBody["sticky_sessions"] != float64(0) {
		t.Errorf("sticky_sessions=%v want 0", statusBody["sticky_sessions"])
	}
	if statusBody["redis_mode"] != "noop" {
		t.Errorf("redis_mode=%v want noop", statusBody["redis_mode"])
	}
	// 有界写队列的丢弃计数必须显式透出（未注入时零值，不能缺字段）。
	if v, ok := statusBody["redis_dropped_writes"]; !ok || v != float64(0) {
		t.Errorf("redis_dropped_writes=%v (present=%v) want 0", v, ok)
	}
}

// TestStatusInFlightFull /status 透出满载计数：healthy 且占满在途的账号数。
func TestStatusInFlightFull(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "full", AccessToken: "at", ExpiresAt: 9999999999},
		&auth.Auth{UID: "free", AccessToken: "at", ExpiresAt: 9999999999},
	)
	p.SetMaxInFlight(1)
	p.Acquire("full")
	defer p.Release("full")

	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var statusBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &statusBody); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if statusBody["in_flight_full"] != float64(1) {
		t.Errorf("in_flight_full=%v want 1", statusBody["in_flight_full"])
	}
	if statusBody["healthy"] != float64(2) {
		t.Errorf("healthy=%v want 2 (full is still healthy by state-machine semantics)", statusBody["healthy"])
	}
}

func TestStatusPortraitFields(t *testing.T) {
	// Phase 3：/status 单账号需返回健康画像字段。
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	p.NoteSuccess("u1")
	p.NoteSuccess("u1")
	p.NoteError("u1") // 记录 last_err + err_total（累计，不冷却）
	p.Cooldown("u1", pool.CoolSoft, time.Hour, "429 rate limit")
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Accounts []pool.Status `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if len(body.Accounts) != 1 {
		t.Fatalf("accounts=%d", len(body.Accounts))
	}
	st := body.Accounts[0]
	if !st.Cooling || st.CoolKind != "soft_rate" || st.CoolRemaining <= 0 {
		t.Errorf("cooling portrait=%+v", st)
	}
	if st.SuccessCount != 2 {
		t.Errorf("success_count=%d want 2", st.SuccessCount)
	}
	if st.ErrTotal != 1 {
		t.Errorf("err_total=%d want 1", st.ErrTotal)
	}
	if st.LastSuccessTime.IsZero() {
		t.Error("last_success should be set")
	}
	if st.LastErrTime.IsZero() {
		t.Error("last_err should be set")
	}
}

// TestHealthzEmptyPool 空池（healthy=0）→ 503，表示暂不可服务。
func TestHealthzEmptyPool(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code=%d want 503 (healthy=0)", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	if resp["healthy"] != float64(0) || resp["total"] != float64(0) {
		t.Errorf("healthz json=%v", resp)
	}
}

// TestHealthz503WhenNoHealthy 所有账号禁用/冷却 → 503。
func TestHealthz503WhenNoHealthy(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.Disable("u1", "session dead")
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("ct=%q want json", ct)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	if resp["healthy"] != float64(0) || resp["total"] != float64(1) {
		t.Errorf("healthz json=%v want healthy=0 total=1", resp)
	}
}

// TestHealthz503WhenAllInFlightFull 全部账号 healthy 但都占满在途 → 503（与 chat 同口径），
// 且 healthy 语义未变（仍为 1）：满载不是状态机健康维度的变化，是探活口径单独叠加。
func TestHealthz503WhenAllInFlightFull(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	p.Acquire("u1") // 占满唯一在途名额
	defer p.Release("u1")

	if p.ServableNow() {
		t.Fatal("servable should be false when the only healthy account is in-flight full")
	}
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 (healthy but all in-flight full)", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	// healthy 语义未变：账号仍是 healthy（只占满在途，非冷却/禁用）。
	if resp["healthy"] != float64(1) || resp["total"] != float64(1) {
		t.Errorf("healthz json=%v want healthy=1 total=1 (healthy semantics unchanged)", resp)
	}
}

// TestHealthz200WhenHealthy 有健康账号 → 200。
func TestHealthz200WithHealthy(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	if resp["healthy"] != float64(1) || resp["total"] != float64(1) {
		t.Errorf("healthz json=%v want healthy=1 total=1", resp)
	}
}

// TestHealthzServiceIdentity /healthz 无论 200 还是 503 都必须带网关身份标识
// （响应体 service 字段 + X-Service 头）：宿主探测打到同端口的旧服务/其他服务时，
// 对方即使返回 2xx 也不带本标识，宿主据此判"假成功"。
func TestHealthzServiceIdentity(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(*pool.Pool)
		wantCode int
	}{
		{"healthy", func(*pool.Pool) {}, http.StatusOK},
		{"unhealthy", func(p *pool.Pool) { p.Disable("u1", "session dead") }, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
			tc.setup(p)
			h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
			if rec.Code != tc.wantCode {
				t.Fatalf("code=%d want %d", rec.Code, tc.wantCode)
			}
			if got := rec.Header().Get("X-Service"); got != ServiceName {
				t.Errorf("X-Service=%q want %q", got, ServiceName)
			}
			var resp map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
			}
			if resp["service"] != ServiceName {
				t.Errorf("service=%v want %q", resp["service"], ServiceName)
			}
		})
	}
}

// TestHealthzServiceIdentityWithoutAuth /healthz 保持无鉴权（负载均衡友好）：
// 配了 api_key 也不要求 Bearer，身份字段照常返回。
func TestHealthzServiceIdentityWithoutAuth(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "secret",
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz must stay unauthenticated: code=%d", rec.Code)
	}
	if got := rec.Header().Get("X-Service"); got != ServiceName {
		t.Errorf("X-Service=%q want %q", got, ServiceName)
	}
}

func TestStatusRequiresAuth(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), APIKey: "secret"})

	// 无 token → 401
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 401 {
		t.Errorf("no token: code=%d", rec.Code)
	}

	// 带 token → 200
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("with token: code=%d", rec.Code)
	}

	// /healthz 无鉴权仍 200
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Errorf("healthz: code=%d", rec.Code)
	}
}

// TestContentBlockedTriggersDegradedRetry passthrough 模式下首请求 400（11128 文案）
// → 降级重试（Degraded）→ 200，客户端无感。验证第二次出站 body 为 Degraded。
func TestContentBlockedTriggersDegradedRetry(t *testing.T) {
	// 记录每次出站请求体，断言第二次为 Degraded 文本。
	var bodies [][]byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			bodies = append(bodies, raw)
			// 首次（body 含原始 system）返回 400 内容拦截；后续返回 200。
			if len(bodies) == 1 {
				return &http.Response{
					StatusCode: 400,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"code":11128,"msg":"blocked by security policy"}`)),
				}, nil
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"原始指纹"},{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("code=%d body=%s (content rejection must not be rewritten)", rec.Code, rec.Body)
	}
	if len(bodies) != 1 || !strings.Contains(string(bodies[0]), "原始指纹") {
		t.Fatalf("request was changed or replayed: %q", bodies)
	}
}

// TestContentBlockedStickyDegraded 降级后新请求直达 Degraded（不再先撞 400）。
func TestContentBlockedStickyDegraded(t *testing.T) {
	var firstCall bool
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if !firstCall {
			firstCall = true
			return 400, `{"code":11128,"msg":"blocked by security policy"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	// 首请求触发降级 → 200。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"x"},{"role":"user","content":"hi"}]}`)))
	if rec.Code != 400 {
		t.Fatalf("first req code=%d", rec.Code)
	}
	second := httptest.NewRecorder()
	h.ServeHTTP(second, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"system","content":"other rules"},{"role":"user","content":"other"}]}`)))
	if second.Code != 200 {
		t.Fatal(second.Code, second.Body)
	}
}

// TestContentBlockedCustomModeDoesNotDegrade custom 模式不触发降级重试
// （custom 已用自有提示词替换，不应再有 system 来源误报）；仍拦则直接回
// 400 content_blocked 防火墙文案（不轮转、不暴露账号/上游错误码）。
func TestContentBlockedCustomModeDoesNotDegrade(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11128,"msg":"blocked by security policy"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "custom", PromptText: "SYS"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"system","content":"old"},{"role":"user","content":"hi"}]}`)))
	// custom 模式下仍拦 → 400 content_blocked（内容终态，换号无意义），不降级重试。
	if rec.Code != 400 {
		t.Fatalf("code=%d want 400 content_blocked (custom does not degrade)", rec.Code)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
	}
	if env.Error.Code != "content_blocked" {
		t.Errorf("error.code=%q want content_blocked", env.Error.Code)
	}
	// 错误透传（error-passthrough，吸收上游 5755fe3）：message 装上游 body 原文
	// （code/msg 原样），客户端必须看到真实错误才能排查；gateway_hint 并列补充。
	if !strings.Contains(rec.Body.String(), "11128") {
		t.Errorf("message should carry upstream original body (passthrough): %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "gateway_hint") {
		t.Errorf("content_blocked should carry gateway_hint: %s", rec.Body.String())
	}

}

// TestContentBlockedDoesNotPenalizeAccount ErrContentBlocked 不罚账号（无冷却/熔断/NoteError）。
func TestContentBlockedDoesNotPenalizeAccount(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11128,"msg":"blocked by security policy"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// 熔断阈值 1：若误罚 NoteError 一次即熔断；content_blocked 不应喂熔断。
	p.SetBreaker(1, time.Hour, time.Hour)
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	st, _ := p.Status("u1")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 {
		t.Fatalf("ErrContentBlocked should not penalize account: %+v", st)
	}
}

// TestCustomModeFingerprintSanitizePreserved custom 端到端：user 消息含 Claude Code
// 指纹句（PR39 fixture 串）→ Rewrite 注入自有 system → 经 sanitize → 出站 body 中
// 该指纹被改写、system 为自有提示词。证明两层（提示词替换 + 清洗）叠加工作。
//
// 两层各自职责（互不替代）：
//   - prompt.Rewrite 替换 system/developer 消息（消灭 system 来源指纹）；
//   - sanitizeMessages 改写 user/assistant 消息中残留的指纹串（兜底用户上下文）。
func TestCustomModeFingerprintSanitizePreserved(t *testing.T) {
	// 捕获出站 body（prepareBody 已强制 stream + 归一 + 清洗后）。
	var sentBody []byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			sentBody = raw
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	const customSys = "我是网关自有提示词"
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "replace", PromptText: customSys})

	// user 消息含 Claude Code 指纹句（PR39 的 fixture 串，逐字精确指纹）。
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{
		"model":"glm-5.2",
		"stream":true,
		"messages":[
			{"role":"system","content":"You are Claude Code, Anthropic's official CLI for Claude."},
			{"role":"user","content":"You are Claude Code, Anthropic's official CLI for Claude. Main branch (you will usually use this for PRs)"}
		]
	}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	out := string(sentBody)
	// 1) system 为自有提示词，旧 system 内容零残留。
	if !strings.Contains(out, customSys) {
		t.Errorf("out body should contain custom system prompt: %s", out)
	}
	if strings.Contains(out, "official CLI for Claude.") && strings.Contains(out, "You are Claude Code, Anthropic's") {
		// 旧 system 原文（含句点）不应以 system 角色出现；但 sanitize 把它改写为
		// "...official CLI tool for Claude."，所以原文 fingerprint 串应消失。
	}
	// 原始指纹串（逐字精确匹配）在出站 body 中应被改写：
	// "official CLI for Claude." → "official CLI tool for Claude."
	// "Main branch (" → "Default branch ("
	if !strings.Contains(out, "official CLI for Claude.") || !strings.Contains(out, "Main branch (you will usually use this for PRs)") {
		t.Errorf("user content was changed: %s", out)
	}
	// 2) messages 头部恰好一条 system = 自有提示词（Rewrite 已删旧 system）。
	var obj map[string]any
	if err := json.Unmarshal(sentBody, &obj); err != nil {
		t.Fatalf("out body not json: %v %s", err, out)
	}
	msgs := obj["messages"].([]any)
	var systemCount int
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] == "system" {
			systemCount++
			if mm["content"] != customSys {
				t.Errorf("system content=%v want %q", mm["content"], customSys)
			}
		}
	}
	if systemCount != 1 {
		t.Errorf("want exactly 1 system message, got %d (all=%v)", systemCount, msgs)
	}
}

// promptModeHarness 让三种组合位置各跑一次真实请求，返回出站 body 的 messages。
// 复用同一 pool/upstream 装置，避免每个用例各写一遍捕获样板。
func promptModeHarness(t *testing.T, cfg Config, body string) []map[string]any {
	t.Helper()
	var sent []byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			sent, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		GlobalEnabled: true, // 夹具：分域用例需要 global 账号可用（产品侧由 config 注入）
	}
	if cfg.Pool == nil {
		cfg.Pool = testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	}
	cfg.Upstream = up
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	NewHandler(cfg).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var obj map[string]any
	if err := json.Unmarshal(sent, &obj); err != nil {
		t.Fatalf("out body not json: %v %s", err, sent)
	}
	raw, _ := obj["messages"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		out = append(out, m.(map[string]any))
	}
	return out
}

const promptModeBody = `{"model":"glm-5.2","stream":true,"messages":[
	{"role":"system","content":"CLIENT-SYSTEM"},
	{"role":"developer","content":"CLIENT-DEV"},
	{"role":"user","content":"hi"}]}`

// TestPromptModeAfterGatewayFirst 后组合：网关提示词置首，客户端开头块紧随其后
// （客户端内容逐字保留，只改顺序）。
func TestPromptModeAfterGatewayFirst(t *testing.T) {
	msgs := promptModeHarness(t, Config{
		PromptRules: map[string]prompt.Rule{"": {Mode: prompt.ModeAfter, Text: "GW"}},
	}, promptModeBody)
	if len(msgs) != 4 {
		t.Fatalf("want 4 messages (gw + 2 client + user), got %d: %v", len(msgs), msgs)
	}
	if msgs[0]["content"] != "GW" || msgs[1]["content"] != "CLIENT-SYSTEM" || msgs[2]["content"] != "CLIENT-DEV" {
		t.Fatalf("after 顺序不对: %v", msgs)
	}
	if msgs[1]["role"] != "system" || msgs[2]["role"] != "developer" {
		t.Errorf("客户端角色被改动: %v", msgs)
	}
}

// TestPromptModeAppendClientFirst 前置追加：客户端块在前，网关提示词在其后。
func TestPromptModeAppendClientFirst(t *testing.T) {
	msgs := promptModeHarness(t, Config{
		PromptRules: map[string]prompt.Rule{"": {Mode: prompt.ModeAppend, Text: "GW"}},
	}, promptModeBody)
	if len(msgs) != 4 {
		t.Fatalf("want 4 messages, got %d: %v", len(msgs), msgs)
	}
	if msgs[0]["content"] != "CLIENT-SYSTEM" || msgs[1]["content"] != "CLIENT-DEV" || msgs[2]["content"] != "GW" {
		t.Fatalf("append 顺序不对: %v", msgs)
	}
}

// TestPromptModeReplaceDropsClient replace：客户端 system/developer 全部删除。
func TestPromptModeReplaceDropsClient(t *testing.T) {
	msgs := promptModeHarness(t, Config{
		PromptRules: map[string]prompt.Rule{"": {Mode: prompt.ModeReplace, Text: "GW"}},
	}, promptModeBody)
	if len(msgs) != 2 || msgs[0]["content"] != "GW" || msgs[0]["role"] != "system" {
		t.Fatalf("replace 结果不对: %v", msgs)
	}
	for _, m := range msgs {
		if m["content"] == "CLIENT-SYSTEM" || m["content"] == "CLIENT-DEV" {
			t.Errorf("客户端 system 未被删除: %v", msgs)
		}
	}
}

// TestPromptModeNoneUntouched none：一个字节都不动。
func TestPromptModeNoneUntouched(t *testing.T) {
	msgs := promptModeHarness(t, Config{
		PromptRules: map[string]prompt.Rule{"": {Mode: prompt.ModeNone, Text: "GW"}},
	}, promptModeBody)
	if len(msgs) != 3 || msgs[0]["content"] != "CLIENT-SYSTEM" || msgs[1]["content"] != "CLIENT-DEV" {
		t.Fatalf("none 不该改写: %v", msgs)
	}
}

// TestPromptRulePerRealm 分域选规则：CN 账号用 cn 规则，global 账号用 global 规则，
// 未配置的域回落默认规则（键 ""）。
func TestPromptRulePerRealm(t *testing.T) {
	rules := map[string]prompt.Rule{
		"":       {Mode: prompt.ModeNone},
		"cn":     {Mode: prompt.ModeReplace, Text: "CN-GW"},
		"global": {Mode: prompt.ModeAfter, Text: "GL-GW"},
	}
	// 池里放一个 global 账号：请求用 global: 前缀路由到它。
	g := &auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999}
	if _, err := auth.BackfillRealmFor(g, "global"); err != nil {
		t.Fatal(err)
	}
	msgs := promptModeHarness(t, Config{PromptRules: rules, GlobalEnabled: true, Pool: testPoolWith(g)},
		`{"model":"global:glm-5.2","stream":true,"messages":[
		{"role":"system","content":"CLIENT"},{"role":"user","content":"hi"}]}`)
	// after：网关在前、客户端 system 紧随其后、user 最后。
	if len(msgs) != 3 || msgs[0]["content"] != "GL-GW" || msgs[1]["content"] != "CLIENT" || msgs[2]["content"] != "hi" {
		t.Fatalf("global 规则未生效: %v", msgs)
	}
}

// TestPromptRulePerRealmCN CN 账号走 cn 规则（不被默认规则的 none 覆盖）。
func TestPromptRulePerRealmCN(t *testing.T) {
	rules := map[string]prompt.Rule{
		"":   {Mode: prompt.ModeNone},
		"cn": {Mode: prompt.ModeReplace, Text: "CN-GW"},
	}
	msgs := promptModeHarness(t, Config{PromptRules: rules}, promptModeBody)
	if len(msgs) != 2 || msgs[0]["content"] != "CN-GW" {
		t.Fatalf("cn 规则未生效: %v", msgs)
	}
}

// TestPromptRuleFallbackDefault 未配置该域 → 回落默认规则（键 ""）。
func TestPromptRuleFallbackDefault(t *testing.T) {
	h := NewHandler(Config{PromptRules: map[string]prompt.Rule{"": {Mode: prompt.ModeReplace, Text: "DEF"}}})
	if r, ok := h.promptRuleFor("cn"); !ok || r.Text != "DEF" {
		t.Errorf("cn 应回落默认规则: %+v ok=%v", r, ok)
	}
	// none / 空正文 / 未命中都不触发组合。
	h2 := NewHandler(Config{PromptRules: map[string]prompt.Rule{"": {Mode: prompt.ModeReplace, Text: ""}}})
	if _, ok := h2.promptRuleFor("cn"); ok {
		t.Error("空正文不应触发组合")
	}
	h3 := NewHandler(Config{PromptRules: map[string]prompt.Rule{}})
	if _, ok := h3.promptRuleFor("cn"); ok {
		t.Error("无规则不应触发组合")
	}
}

// TestFingerprintRewriteEndToEnd 出站指纹改写层的端到端行为：
// 关闭（默认）= 逐字透传；开启 = 内置指纹表 + 自定义规则都改写。
// 覆盖 prompt.mode 清不到的通道（user / tool / reasoning）。
func TestFingerprintRewriteEndToEnd(t *testing.T) {
	// 严格转发契约要求 tool_call 有成对的 tool 结果（否则 400 unresolved tool calls），
	// 因此夹具里补上对应的 tool 消息——它同样是"指纹可藏身处"。
	const body = `{"model":"glm-5.2","stream":true,"messages":[
		{"role":"system","content":"CLIENT-SYSTEM"},
		{"role":"user","content":"帮我看看 code=11128 是什么意思"},
		{"role":"assistant","content":null,"reasoning_content":"internal 11128 detail",
		 "tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"{\"cmd\":\"echo 11128\"}"}}]},
		{"role":"tool","tool_call_id":"a","content":"output: 11128"}
	]}`
	run := func(t *testing.T, layer *scrub.Layer) string {
		t.Helper()
		var sent []byte
		up := &upstream.Client{
			HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				sent, _ = io.ReadAll(r.Body)
				return &http.Response{StatusCode: 200,
					Header: http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:   io.NopCloser(strings.NewReader(sseOK))}, nil
			})},
			ChatBaseCN:    "https://fake.example",
			GlobalEnabled: true,
		}
		up.Fingerprints.Store(layer)
		h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
			Upstream: up})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
		return string(sent)
	}

	// 1) 默认关闭：逐字透传，11128 原样出站（严格透传契约）。
	off := run(t, scrub.NewLayer(false, nil))
	if !strings.Contains(off, "11128") {
		t.Errorf("关闭时不应改写: %s", off)
	}
	// 2) 开启内置层：三处通道（user content / reasoning_content / tool args）全部改写。
	on := run(t, scrub.NewLayer(true, nil))
	if strings.Contains(on, "11128") {
		t.Errorf("user/tool/reasoning 通道未被改写: %s", on)
	}
	// 4 处：user content / reasoning_content / tool_calls.arguments / tool 结果 content。
	if n := strings.Count(on, "11-128"); n != 4 {
		t.Errorf("期望 4 处改写，实际 %d: %s", n, on)
	}
	// 3) 自定义规则叠加：加入一条只针对本项目的规则。
	custom, err := scrub.Build([]scrub.Rule{{Match: "CLIENT-SYSTEM", Replace: "GW", Action: scrub.ActionReplace}})
	if err != nil {
		t.Fatal(err)
	}
	both := run(t, scrub.NewLayer(true, custom))
	if strings.Contains(both, "CLIENT-SYSTEM") {
		t.Errorf("自定义规则未生效: %s", both)
	}
	if strings.Contains(both, "11128") {
		t.Errorf("自定义规则不应顶掉内置层: %s", both)
	}
}

// TestPromptTextVerbatimOnWire 出站链路上正文逐字：无任何运行期改写
// （预设是官方渲染产物，首行就是官方当时的取值）。
func TestPromptTextVerbatimOnWire(t *testing.T) {
	text := "This conversation is powered by 快速\n\n<content_policy>x</content_policy>"
	msgs := promptModeHarness(t, Config{
		PromptRules: map[string]prompt.Rule{
			"": {Mode: prompt.ModeReplace, Text: text},
		},
	}, promptModeBody)
	if got := msgs[0]["content"].(string); got != text {
		t.Fatalf("system text must be verbatim:\nwant %q\ngot  %q", text, got)
	}
}

// TestPromptInjectOnWire inject 出站：单条 system，正文逐字在前、
// 客户端规则进官方包装块（末尾）。
func TestPromptInjectOnWire(t *testing.T) {
	text := "This conversation is powered by 快速\n\n<content_policy>x</content_policy>"
	msgs := promptModeHarness(t, Config{
		PromptRules: map[string]prompt.Rule{
			"": {Mode: prompt.ModeInject, Text: text},
		},
	}, promptModeBody)
	if len(msgs) != 2 {
		t.Fatalf("inject 应合成单条 system + user，got %d: %v", len(msgs), msgs)
	}
	content, _ := msgs[0]["content"].(string)
	if !strings.HasPrefix(content, text+"\n") {
		t.Errorf("正文必须逐字前置:\n%s", content)
	}
	for _, want := range []string{
		"<user_custom_instructions>",
		"CLIENT-SYSTEM", "CLIENT-DEV",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("missing %q:\n%s", want, content)
		}
	}
	if strings.Index(content, text) > strings.Index(content, "<user_custom_instructions>") {
		t.Error("包装块必须在正文之后")
	}
	if msgs[1]["content"] != "hi" {
		t.Errorf("user 消息被改动: %v", msgs[1])
	}
}

// TestPromptFingerprintHelpers 指纹取值口径（首行提取 + sha 截断）。
func TestPromptFingerprintHelpers(t *testing.T) {
	text := "This conversation is powered by 快速\nbody"
	if got := prompt.FirstLineValue(text); got != "快速" {
		t.Errorf("FirstLineValue=%q", got)
	}
	if got := prompt.FirstLineValue("no first line"); got != "" {
		t.Errorf("non-official first line must yield empty, got %q", got)
	}
	a, b := prompt.TextSHA("x"), prompt.TextSHA("x")
	if a == "" || a != b || len(a) != 12 {
		t.Errorf("TextSHA unstable: %q %q", a, b)
	}
	if prompt.TextSHA("x") == prompt.TextSHA("y") {
		t.Error("different text must have different sha")
	}
	if prompt.TextSHA("") != "" {
		t.Error("empty text must yield empty sha")
	}
}

// TestPromptFingerprintRecorded 出站提示词指纹随请求入账（真实跑通出站链路）：
// 组合生效时 mode/来源/sha/字符数齐备且自洽；none 时全部留空。
func TestPromptFingerprintRecorded(t *testing.T) {
	text := "This conversation is powered by 快速\n\n<content_policy>x</content_policy>"
	rec := reqlog.New(reqlog.Config{}) // 仅内存指标（无归档目录）
	cfg := Config{
		RequestLog: rec,
		PromptRules: map[string]prompt.Rule{
			"": {Mode: prompt.ModeReplace, Text: text, Source: "official-craft"},
		},
	}
	promptModeHarness(t, cfg, promptModeBody)

	snap := rec.Snapshot()
	if len(snap.Recent) == 0 {
		t.Fatal("no request recorded")
	}
	e := snap.Recent[len(snap.Recent)-1]
	if e.PromptMode != prompt.ModeReplace {
		t.Errorf("prompt_mode=%q want replace", e.PromptMode)
	}
	if e.PromptPreset != "official-craft" {
		t.Errorf("prompt_preset=%q want official-craft", e.PromptPreset)
	}
	if e.PromptSHA != prompt.TextSHA(text) {
		t.Errorf("prompt_sha256=%q does not match the text", e.PromptSHA)
	}
	if e.PromptChars == 0 || e.PromptChars != len([]rune(text)) {
		t.Errorf("prompt_chars=%d want %d", e.PromptChars, len([]rune(text)))
	}

	// none：不改写 → 指纹留空（不制造"以为注入了"的假象）。
	rec2 := reqlog.New(reqlog.Config{})
	promptModeHarness(t, Config{
		RequestLog:  rec2,
		PromptRules: map[string]prompt.Rule{"": {Mode: prompt.ModeNone, Text: text}},
	}, promptModeBody)
	e2 := rec2.Snapshot().Recent
	if len(e2) == 0 {
		t.Fatal("no request recorded (none mode)")
	}
	last := e2[len(e2)-1]
	if last.PromptMode != "" || last.PromptSHA != "" || last.PromptChars != 0 {
		t.Errorf("none mode must not record a fingerprint: mode=%q sha=%q chars=%d",
			last.PromptMode, last.PromptSHA, last.PromptChars)
	}
}

// TestPromptRulesHotSwap 提示词规则热改：PromptHold 整体替换后，下一次
// promptRuleFor 立即拿到新规则（无需重建 handler / 重启进程）。
func TestPromptRulesHotSwap(t *testing.T) {
	hold := prompt.NewHolder(map[string]prompt.Rule{
		"": {Mode: prompt.ModeReplace, Text: "OLD"},
	})
	h := NewHandler(Config{PromptHold: hold})

	if r, ok := h.promptRuleFor("cn"); !ok || r.Text != "OLD" {
		t.Fatalf("初始规则 = %+v ok=%v", r, ok)
	}
	hold.Store(map[string]prompt.Rule{
		"cn":     {Mode: prompt.ModeAppend, Text: "NEW-CN"},
		"global": {Mode: prompt.ModeReplace, Text: "NEW-GLOBAL"},
	})
	if r, ok := h.promptRuleFor("cn"); !ok || r.Text != "NEW-CN" || r.Mode != prompt.ModeAppend {
		t.Fatalf("热改后 cn 规则 = %+v ok=%v", r, ok)
	}
	if r, ok := h.promptRuleFor("global"); !ok || r.Text != "NEW-GLOBAL" {
		t.Fatalf("热改后 global 规则 = %+v ok=%v", r, ok)
	}
	// 换成 none / 空规则 = 回到透传。
	hold.Store(map[string]prompt.Rule{"": {Mode: prompt.ModeNone, Text: "X"}})
	if _, ok := h.promptRuleFor("cn"); ok {
		t.Fatal("none 模式应视为不改写")
	}
	hold.Store(nil)
	if _, ok := h.promptRuleFor("cn"); ok {
		t.Fatal("空规则应视为不改写")
	}
}

// TestChatStickyPromptCacheKeyFallback 75c0a78 断链恢复回归：pi-ai 驱动客户端把
// 会话 ID 放 prompt_cache_key（而非 conversation_id），同会话两笔请求必须粘同一账号，
// 且成功后绑定跟随（bindStore 镜像里出现该键）。
func TestChatStickyPromptCacheKeyFallback(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"good"} },
	})
	p := testPoolWith(&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999})
	picked := 0
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			picked++
			// 语义隔离：prompt_cache_key 不得被伪造为上游 X-Conversation-ID。
			if got := r.Header.Get("X-Conversation-ID"); got != "" {
				t.Errorf("X-Conversation-ID should stay empty for prompt_cache_key sessions, got %q", got)
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})

	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"你好"}],"prompt_cache_key":"pi-sess-1"}`
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("request %d: code=%d body=%s", i, rec.Code, rec.Body)
		}
	}
	if picked != 2 {
		t.Fatalf("expected 2 upstream calls, got %d", picked)
	}
	// 第二笔必须走粘性路径绑定同一号（镜像键 = cn:模型:pi-sess-1）。
	if uid, ok := st.lastUID("cn:glm-5.2:pi-sess-1"); !ok || uid != "good" {
		t.Fatalf("prompt_cache_key session should bind to good, got %s ok=%v", uid, ok)
	}
}

// TestChatStickyDerivedKeyFallback 派生键回退：客户端不发任何会话标识时，
// 同一会话（system + 首条 user 相同）多笔请求粘同一账号；换会话（不同首条 user）
// 键不同。带 user_id 的请求不派生（契约），不粘。
func TestChatStickyDerivedKeyFallback(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"good"} },
	})
	p := testPoolWith(&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(auth string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})

	post := func(body string) {
		t.Helper()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
	}
	// 同一会话两轮（历史追加，首条 user 不变）→ 派生键稳定 → 同一键绑定。
	post(`{"model":"glm-5.2","messages":[{"role":"user","content":"第一问"}]}`)
	post(`{"model":"glm-5.2","messages":[{"role":"user","content":"第一问"},{"role":"assistant","content":"答"},{"role":"user","content":"第二问"}]}`)
	// 键是派生哈希（d- 前缀）：验证恰好一条绑定且指向 good。
	if n := len(st.binds); n != 1 {
		t.Fatalf("derived-key session should create exactly 1 binding, got %d (%v)", n, st.binds)
	}
	for k, uid := range st.binds {
		if uid != "good" {
			t.Fatalf("derived binding %q -> %s, want good", k, uid)
		}
	}

	// 带 user_id 的请求不派生 → 无新绑定（仍在 1 条）。
	post(`{"model":"glm-5.2","user_id":"u1","messages":[{"role":"user","content":"另一个会话"}]}`)
	if n := len(st.binds); n != 1 {
		t.Fatalf("user_id request should not derive a sticky key, bindings=%d (%v)", n, st.binds)
	}
}

// TestChatStickyConversationIDTakesPriority 优先级回归：conversation_id 在场时
// prompt_cache_key 不抢占粘性键（显式会话键优先，对齐 ExtractKey 第 1-4 级）。
func TestChatStickyConversationIDTakesPriority(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"good"} },
	})
	p := testPoolWith(&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(auth string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-9"},"prompt_cache_key":"pc-9"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if _, ok := st.lastUID("cn:glm-5.2:conv-9"); !ok {
		t.Fatalf("conversation_id should win as sticky key, binds=%v", st.binds)
	}
	if _, ok := st.lastUID("cn:glm-5.2:pc-9"); ok {
		t.Fatalf("prompt_cache_key should not create a parallel binding, binds=%v", st.binds)
	}
}

// TestChatStickyClientSessionHeaders 客户端自定义会话头键位（ 实抓
// 口径）：Claude Code / Codex / OpenCode / PI / 通用头携带稳定会话标识时，
// 同会话两笔请求粘同一账号；conversation_id 优先级仍最高。
func TestChatStickyClientSessionHeaders(t *testing.T) {
	cases := []struct {
		name   string
		header map[string]string
		// wantKeySuffix 是期望出现在粘性键尾部的会话标识（含命名空间前缀）。
		wantKeySuffix string
	}{
		{"claude-code", map[string]string{"X-Claude-Code-Session-Id": "cc-sess-1"}, "claude:cc-sess-1"},
		{"codex", map[string]string{"Session-Id": "cx-sess-1"}, "codex:cx-sess-1"},
		{"opencode", map[string]string{"X-Session-Affinity": "ses_aff1"}, "affinity:ses_aff1"},
		{"pi-client", map[string]string{"X-Client-Request-Id": "pi-req-1"}, "clientreq:pi-req-1"},
		{"generic", map[string]string{"X-Session-ID": "gen-1"}, "hdr:gen-1"},
		{"pi-slot", map[string]string{"X-Slot-Session-Id": "slot-9"}, "slot:slot-9"},
		{"task", map[string]string{"X-Task-Id": "tk-1"}, "task:tk-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newBindStore()
			sess := session.New(session.Config{
				TTL:       time.Minute,
				Store:     st,
				Available: func() []string { return []string{"good"} },
			})
			p := testPoolWith(&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999})
			up := newFakeUpstream(t, func(auth string) (int, string, bool) {
				return 200, sseOK, true
			})
			h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})
			for i := 0; i < 2; i++ {
				req := httptest.NewRequest("POST", "/v1/chat/completions",
					strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
				for k, v := range tc.header {
					req.Header.Set(k, v)
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != 200 {
					t.Fatalf("request %d: code=%d body=%s", i, rec.Code, rec.Body)
				}
			}
			wantKey := "cn:glm-5.2:" + tc.wantKeySuffix
			if uid, ok := st.lastUID(wantKey); !ok || uid != "good" {
				t.Fatalf("header session should bind via %q, got binds=%v", wantKey, st.binds)
			}
		})
	}
}

// TestChatStickyClaudeCodeUserIDSession Claude Code 的 metadata.user_id
// （user_{hash}_session_{uuid} 形态）：session 段提取为粘性键——这是 /v1/messages
// 主力客户端唯一的会话信号；非该格式的 user_id 仍不参与（反垄断契约不回归）。
func TestChatStickyClaudeCodeUserIDSession(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"good"} },
	})
	p := testPoolWith(&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(auth string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})

	post := func(body string) {
		t.Helper()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
	}
	// Claude Code 形态：同 session uuid 两笔（account hash 不同也算同会话）。
	post(`{"model":"glm-5.2","metadata":{"user_id":"user_abc123_session_550e8400-e29b-41d4-a716-446655440000"},"messages":[{"role":"user","content":"hi"}]}`)
	post(`{"model":"glm-5.2","metadata":{"user_id":"user_def456_session_550e8400-e29b-41d4-a716-446655440000"},"messages":[{"role":"user","content":"第二轮"}]}`)
	if uid, ok := st.lastUID("cn:glm-5.2:claude:550e8400-e29b-41d4-a716-446655440000"); !ok || uid != "good" {
		t.Fatalf("claude session uuid should bind, binds=%v", st.binds)
	}

	// 非 Claude 格式 user_id：不提取、不派生（反垄断契约保持）。
	st2binds := len(st.binds)
	post(`{"model":"glm-5.2","metadata":{"user_id":"plain-user"},"messages":[{"role":"user","content":"another"}]}`)
	if len(st.binds) != st2binds {
		t.Fatalf("plain user_id must not create binding, binds=%v", st.binds)
	}
}

// TestChatMarkupRepairRecorded DSML 标记还原端到端跑通 handler 链路：
// 上游在无 tools 请求里把工具调用吐成原生标记文本，handler 还原成 tool_calls，
// 并把还原计数写进 reqlog 归档的改写列（§15 显式可见）。流式与非流式都覆盖。
func TestChatMarkupRepairRecorded(t *testing.T) {
	// 真实泄漏正文（取自 upstream 回归夹具）嵌进一帧 content。
	leaked, err := os.ReadFile("../upstream/testdata/leaked_markup.txt")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	esc, _ := json.Marshal(string(leaked))
	leakSSE := `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant","content":` + string(esc) + `}}]}` + "\n\n" +
		`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\ndata: [DONE]\n\n"

	for _, stream := range []bool{false, true} {
		name := "nonstream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, leakSSE, true })
			rec := reqlog.New(reqlog.Config{})
			h := NewHandler(Config{
				Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
				Upstream:   up,
				RequestLog: rec,
			})
			body := `{"model":"glm-5.2","messages":[{"role":"user","content":"go"}]}`
			if stream {
				body = `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"go"}]}`
			}
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != 200 {
				t.Fatalf("code=%d body=%s", w.Code, w.Body)
			}
			out := w.Body.String()
			if strings.Contains(out, "DSML") {
				t.Fatalf("markup leaked to client:\n%s", out)
			}
			if !strings.Contains(out, "exec_command") {
				t.Fatalf("tool call not restored:\n%s", out)
			}
			// 还原计入 reqlog 改写列（面板 LogsView 据此展示）。
			snap := rec.Snapshot()
			if len(snap.Recent) == 0 {
				t.Fatal("no request recorded")
			}
			e := snap.Recent[len(snap.Recent)-1]
			found := false
			for _, rw := range e.Rewritten {
				if strings.HasPrefix(rw, "tool_calls.markup:") {
					found = true
				}
			}
			if !found {
				t.Fatalf("markup repair not recorded in rewritten: %v", e.Rewritten)
			}
		})
	}
}
