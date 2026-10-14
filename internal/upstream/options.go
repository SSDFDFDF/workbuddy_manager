// options.go 上游出站配置的运行期快照（可热改部分）。
//
// 背景：profiles / client_name / device_token / chat_base / global.enabled / 各档超时
// 原先在装配期直接写进 Client 的导出字段，配置改了必须重启。这里把它们收敛成一份
// **不可变快照**，由 main 装配与配置保存路径整体 Store，请求热路径 Load 一次取用：
// 无锁、无数据竞争，且一次请求内看到的一组值是自洽的（不会新旧混读）。
//
// 与静态字段的兼容：历史调用方（测试/裸用）仍可直接给 Client 的导出字段赋值；
// 只有在 Configure 被调用后快照才存在，此后读取一律以快照为准。静态字段是
// **装配期**输入，运行期（HTTP 已开始服务后）不得再原地写。
package upstream

import (
	"net/http"
	"time"
)

// Options 可热改的上游出站配置快照。
//
// 零值合法：每个字段都有"未设置 = 回落既有默认"的口径（见各读取点）。
type Options struct {
	// Profiles 各域出站身份（cn/global），影响 UA/版本头与缓存键材料。
	Profiles map[string]IdentityProfile
	// ClientName 用量归属头取值（空 = 旧行为）。
	ClientName string
	// DeviceToken / DeviceTokenFile 设备风控 token 的兜底来源（账号级优先）。
	DeviceToken     string
	DeviceTokenFile string
	// PassthroughIP 是否把客户端 IP 透传给上游。
	PassthroughIP bool
	// DeriveConversationID 客户端未提供会话 id 时，是否把网关侧稳定会话键
	//（convKey）派生为 UUIDv4 形态的 X-Conversation-ID 出站（upstream.derive_conversation_id）。
	//
	// 默认 false = 保持历史行为（客户端没给就不发，不伪造）；开启后官方没有的
	// 第三方客户端也能获得上游的“同一会话”身份（会话维度上下文缓存/聚合的前提）。
	DeriveConversationID bool
	// ChatBaseCN CN 域 chat base（空 = 内置默认）。
	ChatBaseCN string
	// ChatBaseGlobal / BillingBaseGlobal global 域 base（空 = 内置默认）。
	ChatBaseGlobal    string
	BillingBaseGlobal string
	// GlobalEnabled global realm 路由总开关（false = 纯 CN 逃生门）。
	GlobalEnabled bool
	// IdleTimeout 聊天 SSE 流中空闲上限（<=0 = 关闭空闲监控）。
	IdleTimeout time.Duration
	// StreamTimeouts SSE 语义阶段上限（零值 = 内置默认）。
	StreamTimeouts StreamTimeoutConfig
	// ShortTimeout 短 RPC（refresh/checkin/balance/FetchModels）总时长上限。
	// 通过短 RPC 派生 client 生效，不改写共享 *http.Client.Timeout。
	ShortTimeout time.Duration
}

// defaultShortTimeout 未配置时的短 RPC 总时长兜底（与 New 的 HTTP.Timeout 一致）。
const defaultShortTimeout = 120 * time.Second

// Configure 整体替换热改快照（装配期与配置保存路径调用）。
// Profiles 会被克隆，保证快照之后的不可变性。
func (c *Client) Configure(o Options) {
	if c == nil {
		return
	}
	if len(o.Profiles) > 0 {
		profiles := make(map[string]IdentityProfile, len(o.Profiles))
		for k, v := range o.Profiles {
			profiles[k] = v
		}
		o.Profiles = profiles
	}
	c.opts.Store(&o)
}

// optsNow 返回当前生效选项：优先热改快照；从未 Configure 时由静态字段合成
// （测试与裸用路径零改动）。
func (c *Client) optsNow() Options {
	if c == nil {
		return Options{}
	}
	if p := c.opts.Load(); p != nil {
		return *p
	}
	return c.staticOptions()
}

// staticOptions 由装配期静态字段合成选项（仅在从未 Configure 时使用）。
func (c *Client) staticOptions() Options {
	o := Options{
		Profiles:          c.Profiles,
		ClientName:        c.ClientName,
		DeviceToken:       c.DeviceToken,
		DeviceTokenFile:   c.DeviceTokenFile,
		PassthroughIP:     c.PassthroughIP,
		ChatBaseCN:        c.ChatBaseCN,
		ChatBaseGlobal:    c.ChatBaseGlobal,
		BillingBaseGlobal: c.BillingBaseGlobal,
		GlobalEnabled:     c.GlobalEnabled,
		IdleTimeout:       c.IdleTimeout,
		StreamTimeouts:    c.StreamTimeouts,
	}
	if c.HTTP != nil {
		o.ShortTimeout = c.HTTP.Timeout
	}
	return o
}

// StreamTimeoutsNow 返回当前 SSE 阶段上限（handler 每请求读取，热改即时生效）。
func (c *Client) StreamTimeoutsNow() StreamTimeoutConfig {
	return c.optsNow().StreamTimeouts
}

// DeriveConversationIDsNow 当前是否开启“派生会话 ID”出站
// （见 Options.DeriveConversationID）。
func (c *Client) DeriveConversationIDsNow() bool {
	return c.optsNow().DeriveConversationID
}

// shortTimeoutNow 当前生效的短 RPC 总时长。
//
// 口径与装配期逐字一致：Configure 过就以快照值为准（0 = 不限，与旧实现的
// up.HTTP.Timeout = 0 等价）；从未 Configure（测试/裸用）时用装配期 client 的
// Timeout（0 仍是"不限"）。
func (c *Client) shortTimeoutNow() time.Duration {
	if c == nil {
		return defaultShortTimeout
	}
	if p := c.opts.Load(); p != nil {
		return p.ShortTimeout
	}
	if c.HTTP != nil {
		return c.HTTP.Timeout
	}
	return defaultShortTimeout
}

// ShortClient 返回短 RPC 使用的 http.Client：在共享 *http.Client 的**副本**上套用
// 当前 ShortTimeout。复制而不是原地改 Timeout——http.Client.Do 会并发读取该字段，
// 原地写是数据竞争。副本是安全的：http.Client 只有 Transport/CheckRedirect/Jar/
// Timeout 四个字段（无锁状态），连接池仍在共享的 Transport 里。
// 每次调用最多一次小结构体复制，相对一次 RPC 的网络往返可忽略。
func (c *Client) ShortClient() *http.Client {
	if c == nil {
		return nil
	}
	base := c.shortClientBase()
	if base == nil {
		return nil
	}
	if t := c.shortTimeoutNow(); base.Timeout != t {
		v := *base
		v.Timeout = t
		return &v
	}
	return base
}

// GlobalOn 报告是否启用 global realm 路由（热改即时生效）。
func (c *Client) GlobalOn() bool {
	return c.optsNow().GlobalEnabled
}
