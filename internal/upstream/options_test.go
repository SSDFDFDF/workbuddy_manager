package upstream

import (
	"net/http"
	"sync"
	"testing"
	"time"
)

// TestConfigureHotSwap 热改快照整体替换：域名/身份/开关/超时下一次读取即生效，
// 且 ShortClient 是共享 Transport 上的副本（不原地改共享 client 的 Timeout）。
func TestConfigureHotSwap(t *testing.T) {
	c := New()
	c.Configure(Options{
		Profiles:          map[string]IdentityProfile{"global": {ClientVersion: "9.9.9"}},
		ClientName:        "hot-name",
		DeviceToken:       "hot-token",
		PassthroughIP:     true,
		ReplayThinking:    true,
		ChatBaseCN:        "https://cn.example",
		ChatBaseGlobal:    "https://g.example",
		BillingBaseGlobal: "https://b.example",
		GlobalEnabled:     false,
		IdleTimeout:       7 * time.Second,
		StreamTimeouts:    StreamTimeoutConfig{FirstModelEvent: 3 * time.Second},
		ShortTimeout:      5 * time.Second,
	})

	if !c.ReplayThinkingNow() {
		t.Fatal("thinking replay not hot-applied")
	}
	if got := c.chatBase(nil); got != "https://cn.example" {
		t.Fatalf("chatBase=%q", got)
	}
	if got := c.globalChatBase(); got != "https://g.example" {
		t.Fatalf("globalChatBase=%q", got)
	}
	if got := c.globalBillingBase(); got != "https://b.example" {
		t.Fatalf("globalBillingBase=%q", got)
	}
	if c.GlobalOn() {
		t.Fatal("GlobalOn 应为 false")
	}
	if got := c.attributionClientName(); got != "hot-name" {
		t.Fatalf("attributionClientName=%q", got)
	}
	if got := c.resolveDeviceToken(nil); got != "hot-token" {
		t.Fatalf("resolveDeviceToken=%q", got)
	}
	if got := c.optsNow().Profiles["global"].ClientVersion; got != "9.9.9" {
		t.Fatalf("profiles hot value=%q", got)
	}
	if got := c.optsNow().IdleTimeout; got != 7*time.Second {
		t.Fatalf("idle=%v", got)
	}
	if got := c.StreamTimeoutsNow().FirstModelEvent; got != 3*time.Second {
		t.Fatalf("stream timeouts=%+v", got)
	}

	// 短 RPC client：Timeout 跟随快照；复制而非原地改；Transport 仍共享。
	sc := c.ShortClient()
	if sc == nil || sc.Timeout != 5*time.Second {
		t.Fatalf("ShortClient.Timeout=%v", sc)
	}
	if sc == c.HTTP {
		t.Fatal("ShortTimeout 与底层不同时应返回副本")
	}
	if sc.Transport != c.HTTP.Transport {
		t.Fatal("副本必须共享 Transport（连接池不重复）")
	}
	if c.HTTP.Timeout == 5*time.Second {
		t.Fatal("不得原地改共享 client 的 Timeout（数据竞争）")
	}

	// 再次整体替换：旧值不得残留。
	c.Configure(Options{ChatBaseCN: "https://cn2.example", ShortTimeout: 9 * time.Second, GlobalEnabled: true})
	if c.ReplayThinkingNow() {
		t.Fatal("replacement must disable thinking replay")
	}
	if got := c.chatBase(nil); got != "https://cn2.example" {
		t.Fatalf("替换后 chatBase=%q", got)
	}
	if !c.GlobalOn() {
		t.Fatal("替换后 GlobalOn 应为 true")
	}
	if got := c.ShortClient().Timeout; got != 9*time.Second {
		t.Fatalf("替换后 short timeout=%v", got)
	}
	// 未配置的字段回落默认（不再带上一轮的值）。
	if got := c.globalChatBase(); got != defaultGlobalBase {
		t.Fatalf("未配置的 global base 应回落默认，得到 %q", got)
	}
	if got := c.attributionClientName(); got != "WorkBuddy" {
		t.Fatalf("未配置的 ClientName 应回落 WorkBuddy，得到 %q", got)
	}
}

// TestConfigureProfilesCloned 快照必须克隆 Profiles：调用方之后原地改 map
// 不能穿透进已生效快照（否则热路径读到半更新状态）。
func TestConfigureProfilesCloned(t *testing.T) {
	c := New()
	profiles := map[string]IdentityProfile{"cn": {ClientVersion: "1.0.0"}}
	c.Configure(Options{Profiles: profiles})
	profiles["cn"] = IdentityProfile{ClientVersion: "2.0.0"}
	if got := c.optsNow().Profiles["cn"].ClientVersion; got != "1.0.0" {
		t.Fatalf("快照被外部改动穿透：%q", got)
	}
}

// TestConfigureConcurrentReads 热改快照的并发安全：写（Configure）与读（各访问器）
// 同时进行不得触发数据竞争（-race 下运行才有意义；普通运行验证不 panic/不撕裂）。
func TestConfigureConcurrentReads(t *testing.T) {
	c := New()
	c.Configure(Options{ChatBaseCN: "https://a.example", GlobalEnabled: true, ShortTimeout: time.Second})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = c.chatBase(nil)
				_ = c.globalChatBase()
				_ = c.globalOn(nil)
				_ = c.GlobalOn()
				_ = c.ReplayThinkingNow()
				_ = c.StreamTimeoutsNow()
				_ = c.attributionClientName()
				_ = c.resolveDeviceToken(nil)
				_ = c.ShortClient()
				_ = c.optsNow()
			}
		}()
	}
	for i := 0; i < 200; i++ {
		c.Configure(Options{
			ChatBaseCN:     "https://a.example",
			ChatBaseGlobal: "https://g.example",
			ClientName:     "n",
			GlobalEnabled:  i%2 == 0,
			ReplayThinking: i%2 == 0,
			ShortTimeout:   time.Duration(i) * time.Millisecond,
			StreamTimeouts: StreamTimeoutConfig{FirstModelEvent: time.Duration(i) * time.Second},
			Profiles:       map[string]IdentityProfile{"cn": {ClientVersion: "1"}},
		})
	}
	close(stop)
	wg.Wait()
}

// TestShortClientSemanticsTimeoutZero 短 RPC client 的超时口径必须与装配期一致：
//   - 快照 ShortTimeout=0 → 不限（与旧实现 up.HTTP.Timeout=0 等价），不偷偷加默认上限；
//   - 从未 Configure 的手工 Client → 沿用装配期 HTTP.Timeout（0 = 不限）；
//   - 值相等时返回原 client（零复制）。
func TestShortClientSemanticsTimeoutZero(t *testing.T) {
	// 手工构造（未 Configure）：Timeout=0 → 原样返回。
	raw := &Client{HTTP: &http.Client{}}
	if got := raw.ShortClient(); got != raw.HTTP {
		t.Fatalf("未 Configure 且 Timeout=0 时应原样返回，得到 %+v", got)
	}

	// Configure 显式 0：仍是不限（副本、Timeout=0）。
	c := New()
	c.Configure(Options{ShortTimeout: 0})
	if got := c.ShortClient(); got.Timeout != 0 {
		t.Fatalf("ShortTimeout=0 应为不限，实际 %v", got.Timeout)
	}
	// Configure 非 0：套用该值，且 Transport 仍共享。
	c.Configure(Options{ShortTimeout: 42 * time.Second})
	sc := c.ShortClient()
	if sc.Timeout != 42*time.Second {
		t.Fatalf("ShortTimeout 未生效：%v", sc.Timeout)
	}
	if sc.Transport != c.baseTransport && sc.Transport != c.proxyDyn.Load() {
		t.Fatal("短 RPC 副本必须共享同一个转发层/连接池")
	}
	// 与底层同值时零复制。
	c.Configure(Options{ShortTimeout: c.HTTP.Timeout})
	if got := c.ShortClient(); got != c.HTTP {
		t.Fatal("超时与底层相同时应返回原 client（零复制）")
	}
}
