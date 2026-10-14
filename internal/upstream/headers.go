// Package headers 构造三类上游请求头（common / chat / billing / refresh）。
// 规则来自 docs/api-reference.md §0/§4/§6。
package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"workbuddy_manager/internal/auth"
	"workbuddy_manager/internal/session"
)

const (
	// defaultClientVersion 出站 WorkBuddy 客户端版本段（UA 的 `WorkBuddy/<ver>` 与
	// 白名单头组的 X-IDE-Version）。对齐官方 WorkBuddy Desktop 分发包版本（5.7.6）。
	// config upstream.client_version 可覆盖（空 = 内置默认）。
	defaultClientVersion = "5.7.6"
	// defaultCliVersion 出站 UA 中 `CLI/<ver>` 段版本。对齐官方内置 CLI（2.156.0）。
	// config upstream.cli_version 可覆盖（空 = 内置默认）。
	defaultCliVersion = "2.156.0"

	originRefererCN     = "https://www.workbuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
)

// originRefererCN/Global 基线域；实际 Origin 取自 IdentityProfile（identity.go）。

// clientVersion 生效的 WorkBuddy 客户端版本（CN 基线 profile）。
func (c *Client) clientVersion() string { return c.identity(nil).ClientVersion }

func (c *Client) cliVersion() string { return c.identity(nil).CLIVersion }

// defaultWorkBuddyUAFor 默认出站 UA，按账号 realm 的 profile 生成。
func (c *Client) defaultWorkBuddyUAFor(a *auth.Auth) string {
	return c.purposeUA(a, "chat")
}

// userAgent 返回当前出站 UA（chat/refresh/catalog 等路径）。
func (c *Client) userAgent(a *auth.Auth) string {
	return c.purposeUA(a, "chat")
}

// resolveDeviceToken 解析本次请求的 X-Device-Token 取值。
// 优先级：auth.Auth.DeviceToken（每号）> Client.DeviceToken（config 全局）> 文件兜底。
// 三者皆空/读失败则返回空串（调用方不注入该头，优雅降级）。
func (c *Client) resolveDeviceToken(a *auth.Auth) string {
	if a != nil && a.DeviceToken != "" {
		return a.DeviceToken
	}
	if c != nil {
		o := c.optsNow()
		if o.DeviceToken != "" {
			return o.DeviceToken
		}
		if o.DeviceTokenFile != "" {
			return readDeviceTokenFile(o.DeviceTokenFile)
		}
	}
	return ""
}

// injectDeviceToken 在 req 注入 X-Device-Token 头（仅当取到非空 token）。
func (c *Client) injectDeviceToken(req *http.Request, a *auth.Auth) {
	if tok := c.resolveDeviceToken(a); tok != "" {
		req.Header.Set("X-Device-Token", tok)
	}
}

// deriveAccountStableID 按 uid + 用途盐稳定派生 36 hex 设备/会话标识。
// 跨重启稳定（固定盐 "wb2a:"，不随进程换——这是与 session 包派生盐的本质差异：
// 那是会话键维度的进程级随机盐，重启换新；本函数是账号维度，必须跨重启恒定）、
// 账号间互异（uid 不同则不同）、同 uid 同用途恒同值（幂等）。用 sha256 与项目
// 既有派生（session/ids.go、cache_key.go）保持一致；截 36 hex 提供更长熵。
//
// 两个用途：
//   - purpose="machine" → X-Machine-ID（设备级，跨会话稳定）
//   - purpose="session" → X-Session-ID（账号固定会话，跨重启稳定）
//
// 与 injectDeviceToken 的 X-Device-Token 并存不冲突：那是登录时上游签发的
// 真实设备令牌（有则发，权威）；本对头是「每账号一台固定虚拟设备」的稳定指纹，
// 防多号被上游按设备指纹缺失/漂移关联风控。两者是不同头族，官方桌面端都发。
func deriveAccountStableID(uid, purpose string) string {
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
	return hex.EncodeToString(sum[:18]) // 36 hex chars
}

// injectAccountStableHeaders 在 req 注入 X-Machine-ID / X-Session-ID：按 uid 稳定
// 派生，跨重启固定、账号间互异。uid 为空时不注入（匿名请求无设备标识，上游不要求）。
func (c *Client) injectAccountStableHeaders(req *http.Request, a *auth.Auth) {
	if a == nil || a.UID == "" {
		return
	}
	req.Header.Set("X-Machine-ID", deriveAccountStableID(a.UID, "machine"))
	req.Header.Set("X-Session-ID", deriveAccountStableID(a.UID, "session"))
}

// CommonHeaders 设置所有 API 共享的请求头。
func (c *Client) CommonHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	// Accept 非流式默认 application/json（D6：去掉宽松的 text/plain, */*）。
	// chat 流式路径在 ChatHeaders 覆盖为 event-stream。
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	origin := c.identity(a).Origin
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", c.userAgent(a))
	// X-CodeBuddy-Request: 1（官方客户端风控闸门头，所有 API 请求必带，D1）。
	req.Header.Set("X-CodeBuddy-Request", "1")
	// Accept-Language 按 realm 切（D5）：CN zh-CN，global en-US。官方客户端按账号域
	// 发对应语言标识，对齐避免上游风控按语言缺失误判。
	req.Header.Set("Accept-Language", c.identity(a).Language)
	// X-Machine-ID / X-Session-ID：按 uid 稳定派生的账号级设备头（见
	// injectAccountStableHeaders）。注入在 CommonHeaders——chat 经 ChatHeaders
	// 叠加 CommonHeaders 天然继承；billing 域另行注入，全出站覆盖。
	c.injectAccountStableHeaders(req, a)
	// Resin 代理池账号标记（未接入时空操作，出站行为零改动）。
	c.tagProxy(req, a)
}

// acceptLanguageFor 按账号 realm 返回 Accept-Language：global → en-US，cn → zh-CN。
func acceptLanguageFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return "en-US"
	}
	return "zh-CN"
}

// injectCodeBuddyRequest 在 req 注入 X-CodeBuddy-Request: 1。
// billing 域未走 CommonHeaders，单独注入保证全出站覆盖（D1）。
func (c *Client) injectCodeBuddyRequest(req *http.Request) {
	req.Header.Set("X-CodeBuddy-Request", "1")
}

// ChatMeta 一次 chat 出站的会话头族元数据（issue #35：后台按 X-Conversation-Request-ID
// 聚合请求，官方客户端一次 user send 内所有 tool call/重试/换号复用同一个 ID）。
// handler 在轮转循环外生成 conversationID / conversationRequestID，循环内每次出站
// 复用同值；TraceID 透传入站值（空 = 回落 conversationRequestID）。
// messageID（消息级，每条独立）由 ChatHeaders 内部生成，无需外部可见。
type ChatMeta struct {
	PrincipalID           string
	ConversationID        string // X-Conversation-ID：body 提取的入站值（或配置开启时的派生值），空则不发
	ConversationRequestID string // X-Conversation-Request-ID / X-Root-Request-ID：聚合主键，必发
	TraceID               string
	RootRequestID         string
	ParentConversationID  string
	AgentType             string // X-Agent-Type：入站透传优先；空时按 parentConversationId 派生 main/subagent
	// AgentPurpose X-Agent-Purpose：入站透传优先（官方在同一条链路用 conversation /
	// conversation_topic 两个值）；空时由 injectAttribution 给出官方缺省 conversation。
	AgentPurpose string
	// ExpertID / ExpertTeamTask：专家模式头族（X-Expert-Id / X-Expert-Team-Task）。
	//
	// 来源：桌面端把“专家插件的内部模型请求头”下发给 CLI（main/server.js 的
	// internalModelRequestHeaders：X-Expert-Id=expertId，团队专家另加
	// X-Expert-Team-Task=true），再由 CLI 注入 chat 请求。客户端传了就要透传——
	// 这两个头决定上游按专家/团队专家链路计费与路由，丢掉等于把专家会话降级为普通会话。
	ExpertID        string
	ExpertTeamTask  string
	Traceparent     string
	B3TraceID       string
	ACPConnectionID string // acp-connection-id：官方客户端会话连接标识（UUIDv4）
}

// ChatHeaders 在 common 之上加 chat 专属的账号头。
// 缺省字段用 X-No-* 约定（与 CodeBuddy 官方 CLI 一致）。
// clientIP 为本次请求的客户端 IP（按参数传递，不读共享字段——避免并发串扰）；
// PassthroughIP=false 或 clientIP 为空时不注入 IP 头。
// meta 为会话头族元数据（纯新增，不改既有头），见 injectConversationHeaders。
func (c *Client) ChatHeaders(req *http.Request, a *auth.Auth, clientIP string, meta ChatMeta) {
	a = a.Snapshot(false)
	c.CommonHeaders(req, a)
	// chat 流式 Accept 覆盖 CommonHeaders 的非流式默认（D6）。
	req.Header.Set("Accept", "application/json, text/event-stream")
	// AccessToken 加锁快照：keepalive 定时刷新会在 a.mu 内改写它，锁外直读构成数据竞争
	// （见 auth.AccessTokenValue 注释）。
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	// 安全红线：绝不在 chat 请求里携带 X-Refresh-Token。
	// 企业与域头按 realm 分发：CN 走既有分支（EnterpriseID/Domain 原样透传，缺省 X-No-*）；
	// global 账号由 injectGlobalChatHeaders 统一覆写为国际客户端形态
	// （X-No-Enterprise-Id=1 声明无企业 + X-Domain=www.workbuddy.ai 声明国际版域），
	// 且不回退 X-Domain 到登录会话原值——对齐 intl 项目出站头。
	if a != nil && !a.IsGlobal() {
		if a.EnterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		} else {
			req.Header.Set("X-No-Enterprise-Id", "1")
		}
		if d := a.DomainValue(); d != "" {
			req.Header.Set("X-Domain", d)
		} else {
			req.Header.Set("X-No-Department-Info", "1")
		}
	} else {
		c.injectGlobalChatHeaders(req, a)
	}
	// 用量归属头：默认伪造 WorkBuddy 桌面端指纹（client_name="SaaS" 还原旧行为）。
	c.injectAttribution(req, a)
	// 官方桌面端标准产品身份与 Agent 意图头（daemon-bootstrap 对齐）。
	req.Header.Set("X-Private-Data", "true")
	req.Header.Set("X-Agent-Intent", "craft")
	// 官方客户端内嵌 Stainless Node SDK 运行时头族（实机抓包对齐）。
	req.Header.Set("x-stainless-arch", "x64")
	req.Header.Set("x-stainless-lang", "js")
	req.Header.Set("x-stainless-os", "Windows")
	req.Header.Set("x-stainless-package-version", "6.25.0")
	req.Header.Set("x-stainless-retry-count", "0")
	req.Header.Set("x-stainless-runtime", "node")
	req.Header.Set("x-stainless-runtime-version", "v22.21.1")
	// 客户端 IP 透传（仅 PassthroughIP=true 且本次请求带 IP）。
	c.injectClientIP(req, clientIP)
	// 设备风控头：auth 每号 > config 全局 > 文件兜底；空则不注入。
	c.injectDeviceToken(req, a)
	// 会话头族（对话/请求/消息/B3 链路），见 injectConversationHeaders。
	c.injectConversationHeaders(req, meta)
	// Resin 代理池账号标记（ChatHeaders 不总经 CommonHeaders 的直接调用方，
	// 这里重复标记幂等，保证 chat 出站必带账号）。
	c.tagProxy(req, a)
}

// injectGlobalChatHeaders global 账号（无企业 ID）的 chat 专属声明头，对齐 intl 项目
// （ANALYSIS-global-chat-solutions.md）：
//   - X-No-Enterprise-Id: 1  个人账号无企业 ID，显式声明（避免上游按缺省/可疑判定）
//   - X-Domain: www.workbuddy.ai  显式声明国际版域（与 Origin/Referer 同域）
//
// 仅 global realm 注入；CN 账号走既有 X-No-Department-Info 等分支，零回归。
func (c *Client) injectGlobalChatHeaders(req *http.Request, a *auth.Auth) {
	if a == nil || !a.IsGlobal() {
		return
	}
	req.Header.Set("X-No-Enterprise-Id", "1")
	req.Header.Set("X-Domain", "www.workbuddy.ai")
}

// injectConversationHeaders 注入官方客户端会话头族（issue #35 后台聚合）。
// 头族分四层，各司其职：
//   - X-Conversation-ID：会话级，多轮稳定（body 的 conversationId）。空则不发——
//     透传客户端原值优先，客户端没给就不伪造，避免误导后台建错会话。
//   - X-Conversation-Request-ID：**对话轮级聚合主键**，必发。一次 user send 内的
//     所有 tool call/重试/换号/降级复用同一个 → 后台按它聚合成一条（不再碎片化）。
//   - X-Conversation-Message-ID = X-Request-ID：消息级，每条独立（32 位 hex）。
//   - X-Root-Request-ID：= conversationRequestID（根请求追踪）。
//   - X-Trace-ID：入站透传或 = conversationRequestID。
//   - X-B3-TraceId / X-B3-SpanId / X-B3-Sampled：链路族。B3 规范只认 16/32 hex
//     TraceId 与 16 hex SpanId；入站 conversationRequestID 非法时 TraceId 回落
//     messageID（恒 32 hex），SpanId 取 messageID[:16]（每消息新）。
func (c *Client) injectConversationHeaders(req *http.Request, meta ChatMeta) {
	convReqID := meta.ConversationRequestID
	if convReqID == "" {
		// 零值 meta（直接调 ChatHeaders 的调用方/测试）也要保证聚合主键必发：
		// 本级补一个 32 hex，调用方（handler）已生成稳定的不走到这里。
		convReqID = session.NewMessageID()
	}
	messageID := session.NewMessageID()
	if meta.ConversationID != "" {
		req.Header.Set("X-Conversation-ID", meta.ConversationID)
	}
	// acp-connection-id：会话连接标识（入站透传优先；缺失时由会话派生稳定 UUID 或新建）。
	acpID := meta.ACPConnectionID
	if acpID == "" && meta.ConversationID != "" {
		h := session.RequestIDForKey("acp:" + meta.ConversationID)
		acpID = fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
	} else if acpID == "" {
		h := session.NewMessageID()
		acpID = fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
	}
	req.Header.Set("acp-connection-id", acpID)

	req.Header.Set("X-Conversation-Request-ID", convReqID)
	req.Header.Set("X-Conversation-Message-ID", messageID)
	req.Header.Set("X-Request-ID", messageID)
	root := meta.RootRequestID
	if root == "" {
		root = convReqID
	}
	req.Header.Set("X-Root-Request-ID", root)
	if meta.ParentConversationID != "" {
		req.Header.Set("X-Parent-Conversation-ID", meta.ParentConversationID)
	}
	// X-Agent-Type：官方 resolveAgentType() **恒发**（team / subagent / main）——
	// 入站透传优先，缺省按官方同款规则派生：有 parentConversationId = subagent，
	// 否则 main。此前缺省时一个都不发，上游只能按“无 agent 类型”归类。
	agentType := meta.AgentType
	if agentType == "" {
		if meta.ParentConversationID != "" {
			agentType = "subagent"
		} else {
			agentType = "main"
		}
	}
	req.Header.Set("X-Agent-Type", agentType)
	// X-Agent-Purpose：入站透传优先（官方同一条链路用 conversation / conversation_topic
	// 两值）；未提供时保持 injectAttribution 写入的官方缺省 conversation。
	if meta.AgentPurpose != "" {
		req.Header.Set("X-Agent-Purpose", meta.AgentPurpose)
	}
	// 专家模式头族：客户端传了才发（不伪造专家身份）。
	if meta.ExpertID != "" {
		req.Header.Set("X-Expert-Id", meta.ExpertID)
	}
	if meta.ExpertTeamTask != "" {
		req.Header.Set("X-Expert-Team-Task", meta.ExpertTeamTask)
	}
	traceID := meta.TraceID
	if traceID == "" {
		traceID = convReqID
	}
	req.Header.Set("X-Trace-ID", traceID)
	// 统一 trace 选举：合法 traceparent → 合法 B3 → 合法 X-Trace-ID → 本请求新建。
	// 三条输出头共享同一 trace；span 每尝试新建。不串接不同 trace。
	traceparentTrace, traceparentSpan := "", ""
	parts := strings.Split(meta.Traceparent, "-")
	if len(parts) == 4 && parts[0] == "00" && len(parts[1]) == 32 && validTraceID(parts[1]) &&
		parts[1] != strings.Repeat("0", 32) && len(parts[2]) == 16 && validTraceID(parts[2]) &&
		parts[2] != strings.Repeat("0", 16) && (parts[3] == "00" || parts[3] == "01") {
		traceparentTrace, traceparentSpan = parts[1], parts[2]
	}
	trace := traceparentTrace
	if trace == "" && validTraceID(meta.B3TraceID) && strings.Trim(meta.B3TraceID, "0") != "" {
		trace = meta.B3TraceID
	}
	if trace == "" && validTraceID(traceID) && strings.Trim(traceID, "0") != "" {
		trace = traceID
	}
	if trace == "" {
		trace = messageID
	}
	trace = strings.ToLower(trace)
	span := traceparentSpan
	if span == "" {
		span = messageID[:16]
	}
	req.Header.Set("X-B3-TraceId", trace)
	req.Header.Set("X-B3-SpanId", messageID[:16])
	req.Header.Set("X-B3-Sampled", "1")
	trace32 := trace
	if len(trace32) == 16 {
		trace32 = strings.Repeat("0", 16) + trace32 // 64-bit B3 映射为左补零的 128-bit W3C
	}
	req.Header.Set("traceparent", "00-"+trace32+"-"+strings.ToLower(span)+"-01")
}

// validTraceID 判断 B3 TraceId 是否合法：16 或 32 位 hex（大小写均可）。
// 官方客户端生成的 conversationRequestId 是 32 位 hex（UUID 去横线），入站透传值
// 可能是任意形状（含横线/超长/非 hex），直接塞进 B3 头会破坏链路关联（issue #35）。
func validTraceID(s string) bool {
	if len(s) != 16 && len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
			return false
		}
	}
	return true
}

// attributionClientName 生效的用量归属名：ClientName 非空取之；
// 空默认 "WorkBuddy"（伪造官方桌面端指纹；显式配 "SaaS" 可还原旧行为）。
func (c *Client) attributionClientName() string {
	if v := c.optsNow().ClientName; v != "" {
		return v
	}
	return "WorkBuddy"
}

// injectAttribution 注入用量归属头（X-Agent-Purpose / X-IDE-* / X-Product）。
// 仅在 chat/completions 路径生效（ChatHeaders 调用）。
//
// 默认（ClientName 空）即对齐官方 WorkBuddy 桌面端指纹：X-Agent-Purpose="conversation"
// + X-IDE-Name/Type/Product="WorkBuddy" + X-IDE-Version=client_version，上游用量归因
// 不再出现 client/agentPurpose 为空的「网关特征」。显式 ClientName="SaaS" 还原旧行为
// （仅 X-Product="SaaS"，不设 X-IDE-*）；配其他值则四头跟随该值。
func (c *Client) injectAttribution(req *http.Request, a *auth.Auth) {
	name := c.attributionClientName()
	if name == "SaaS" {
		req.Header.Set("X-Product", "SaaS")
		return
	}
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", name)
	req.Header.Set("X-IDE-Type", name)
	req.Header.Set("X-IDE-Version", c.identity(a).ClientVersion)
	req.Header.Set("X-Product", name)
}

// injectClientIP 在 PassthroughIP 开启时把 clientIP 参数透传给上游（三等价头）。
func (c *Client) injectClientIP(req *http.Request, clientIP string) {
	if c == nil || !c.optsNow().PassthroughIP || clientIP == "" {
		return
	}
	req.Header.Set("X-Forwarded-For", clientIP)
	req.Header.Set("X-Real-IP", clientIP)
	req.Header.Set("X-Client-IP", clientIP)
}

// ExtractClientIP 从入站请求提取客户端 IP 首段（X-Forwarded-For 首段，回落 X-Real-IP）。
func ExtractClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return strings.TrimSpace(xff[:i])
			}
		}
		return strings.TrimSpace(xff)
	}
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	return ""
}

// BillingHeaders billing 接口请求头。
// UA 语义：默认**不设置**（保持现状，Go 客户端自带默认 UA）；仅当显式配置
// c.UserAgent 非空才覆盖——避免默认路径给 billing 引入新的 UA 指纹。
func (c *Client) BillingHeaders(req *http.Request, a *auth.Auth) {
	a = a.Snapshot(false)
	// AccessToken 加锁快照（同 ChatHeaders：keepalive 可在 a.mu 内改写）。
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	c.injectCodeBuddyRequest(req)
	// Accept-Language 按 realm 切（D5，billing 域未走 CommonHeaders，单独注入）。
	req.Header.Set("Accept-Language", c.identity(a).Language)
	req.Header.Set("User-Agent", c.purposeUA(a, "billing"))
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if d := a.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)
	}
	// 设备风控头：billing 域（report/travel/balance/checkin）同样注入。
	c.injectDeviceToken(req, a)
	// Resin 代理池账号标记（billing 域不走 CommonHeaders，单独注入）。
	c.tagProxy(req, a)
}

// RefreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
//
// 头族对齐官方客户端：客户端的 refreshSession 只显式设 X-Refresh-Token /
// X-Auth-Refresh-Source / enterpriseHeaders(X-Domain)，其余靠全局 auth 请求拦截器
// 自动注入——该拦截器只要请求没设 X-No-Authorization 就会带 Authorization、
// X-User-Id、X-Enterprise-Id、X-Tenant-Id、X-Domain。refresh 请求未设任何 X-No-*，
// 故这五头在真实客户端上全部存在，此处逐个补齐以保持出站指纹同形。
func (c *Client) RefreshHeaders(req *http.Request, a *auth.Auth) {
	c.CommonHeaders(req, a)
	req.Header.Set("User-Agent", c.purposeUA(a, "refresh"))
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	// Authorization 在 refresh 端点是「顺带声明」而非鉴权权威（权威是 X-Refresh-Token），
	// 因此即使 accessToken 已过期也照发：客户端正是这么做的——它的业务 API 401
	// 拦截器会在 token 刚被上游判为无效后立即调 refreshSession()，那次刷新请求依然
	// 带着这个已失效的 Bearer（拦截器无条件注入）且能成功续期，反证上游在 refresh
	// 端点不校验 Authorization。故此处与客户端同形，不按过期与否分叉。
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	// X-Domain 对齐官方客户端 refresh 的 enterpriseHeaders(auth)：优先账号落盘 domain，
	// 缺失时回落到 refresh 端点 host（客户端用 endpoint.authority 兑底，此处同语义）。
	// 客户端 refresh 请求总带 X-Domain，补齐此头避免上游按域缺失差异化对待。
	dom := a.DomainValue()
	if dom == "" {
		if u, err := url.Parse(c.chatBase(a)); err == nil && u.Host != "" {
			dom = u.Host
		}
	}
	if dom != "" {
		req.Header.Set("X-Domain", dom)
	}
	// X-Auth-Refresh-Source 对齐官方客户端 refresh 渠道标识 "plugin"（D3）。
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
}
