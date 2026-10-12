package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"workbuddy_manager/internal/auth"
	"workbuddy_manager/internal/pool"
	"workbuddy_manager/internal/upstream"
)

// TestRunTokenRefreshOnlyNearExpiry 兜底扫描只碰「剩余寿命不足 TokenRefreshWindow」的号：
// 接近过期的刷、剩余充足的跳过、无 refreshToken 的跳过。
// 这是该扫描存在的意义——它必须能在无流量/keepalive 关闭时单独把到期号救回来，
// 同时常态下（号都很新鲜）零上游调用。
func TestRunTokenRefreshOnlyNearExpiry(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	near := &auth.Auth{UID: "near", AccessToken: "old", RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	fresh := &auth.Auth{UID: "fresh", AccessToken: "old", RefreshToken: "rt", ExpiresAt: time.Now().Add(30 * 24 * time.Hour).Unix()}
	noRT := &auth.Auth{UID: "nort", AccessToken: "old", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	p.Add(near)
	p.Add(fresh)
	p.Add(noRT)

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	s.RunTokenRefreshNow()

	if got := f.refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls=%d want 1（只有 near 号该被刷）", got)
	}
	if near.AccessToken != "new" {
		t.Errorf("near token 未更新: %s", near.AccessToken)
	}
	if fresh.AccessToken != "old" {
		t.Errorf("剩余 30d 的号不该被刷: %s", fresh.AccessToken)
	}
	if noRT.AccessToken != "old" {
		t.Errorf("无 refreshToken 的号不该被刷: %s", noRT.AccessToken)
	}
}

// TestRunTokenRefreshNoExpiryAlwaysRefreshed ExpiresAt<=0（无过期信息）视为「需刷新」，
// 与请求路径 RefreshSkew 前置刷新同一语义（NeedsRefresh 的既有判据），不会把号漏掉。
func TestRunTokenRefreshNoExpiryAlwaysRefreshed(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt"} // ExpiresAt=0
	p.Add(a)

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	s.RunTokenRefreshNow()

	if got := f.refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls=%d want 1（无过期信息应刷新）", got)
	}
}

// TestRunTokenRefreshSharesSessionDeadCounting 兜底扫描与 keepalive 共享同一套
// 12153 连续计数语义：3 次才禁用（P0-1 误判防护），不会因为换了入口就各算一套。
func TestRunTokenRefreshSharesSessionDeadCounting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
	}))
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	p.Add(a)

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RunTokenRefreshNow()
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("第 1 次 12153 不应禁用: %+v", st)
	}
	s.RunTokenRefreshNow()
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("第 2 次 12153 不应禁用: %+v", st)
	}
	s.RunTokenRefreshNow()
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Fatalf("第 3 次连续 12153 应禁用: %+v", st)
	}
}

// TestRunTokenRefreshSkipsWhenKeepaliveRunning 允许扫描与保活重叠会把 12153 连续
// 计数一次故障算两次（NoteSessionDead 无条件 ++），把 3 次阈值实际降成 2 次——
// 保活用 Lock 占住 refreshMu 时，扫描必须整轮跳过（不刷、不喂计数）。
func TestRunTokenRefreshSkipsWhenKeepaliveRunning(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	p.Add(a)

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	// 模拟「保活正在刷新」：手动占住互斥锁。
	s.refreshMu.Lock()
	s.RunTokenRefreshNow()
	if got := f.refreshCalls.Load(); got != 0 {
		t.Fatalf("保活进行中时扫描不应发起刷新：calls=%d", got)
	}
	s.refreshMu.Unlock()

	// 锁释放后恢复正常。
	s.RunTokenRefreshNow()
	if got := f.refreshCalls.Load(); got != 1 {
		t.Fatalf("锁释放后扫描应正常刷新：calls=%d want 1", got)
	}
}

// TestTokenRefreshImmediateFirstPass 启动后应**立即**扫一遍，而不是等满一个间隔：
// 否则首轮要等 10min，而进程若重启得比间隔还频，timer 每次都被重建、永远等不到触发，
// 「覆盖无流量账号」的承诺就空了。用 1h 间隔 + 短 deadline 断言首次扫描已发生。
func TestTokenRefreshImmediateFirstPass(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	p.Add(a)

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartTokenRefresh(ctx, time.Hour) // 间隔远大于 deadline：只有「立即首扫」能命中

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && f.refreshCalls.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if f.refreshCalls.Load() == 0 {
		t.Fatal("启动后未立即扫描（首轮被间隔阻塞）")
	}
}

// TestTokenRefreshHotEnableAfterDisabledStart 守护「关闭时启动 + 面板热开启」：
// interval<=0 时循环仍须存在并在等待（消费 rearmTokenRefresh），否则面板显示已开启、
// 实际永无 goroutine 扫描。与 TestBalanceRefreshHotEnableAfterDisabledStart 同构。
func TestTokenRefreshHotEnableAfterDisabledStart(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	p.Add(a)

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 关闭状态启动（schedule.token_refresh_enabled=false）。
	s.StartTokenRefresh(ctx, 0)
	time.Sleep(50 * time.Millisecond)
	if got := f.refreshCalls.Load(); got != 0 {
		t.Fatalf("关闭状态不应刷新：calls=%d", got)
	}

	// 面板热开启：间隔 10ms，等待若干轮。
	s.SetTokenRefreshInterval(10 * time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && f.refreshCalls.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if f.refreshCalls.Load() == 0 {
		t.Fatal("热开启后 token 兜底刷新循环未运行")
	}

	// 再热关闭：不再产生新调用。
	s.SetTokenRefreshInterval(0)
	time.Sleep(30 * time.Millisecond)
	before := f.refreshCalls.Load()
	time.Sleep(60 * time.Millisecond)
	if got := f.refreshCalls.Load(); got != before {
		t.Fatalf("暂停后不应继续刷新：before=%d after=%d", before, got)
	}
}
