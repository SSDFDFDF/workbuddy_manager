// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"workbuddy_manager/internal/auth"
	"workbuddy_manager/internal/config/runtime"
	"workbuddy_manager/internal/forwarding"
	"workbuddy_manager/internal/httpauth"
	"workbuddy_manager/internal/jsondoc"
	"workbuddy_manager/internal/logfmt"
	"workbuddy_manager/internal/media"
	"workbuddy_manager/internal/pool"
	"workbuddy_manager/internal/prompt"
	"workbuddy_manager/internal/protocol"
	"workbuddy_manager/internal/reqlog"
	"workbuddy_manager/internal/session"
	"workbuddy_manager/internal/upstream"
	"workbuddy_manager/internal/usage"
)

// Config handler 依赖。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	APIKey    string // 空 = 不鉴权（静态值；与 Live 同时给出时 Live 优先）
	MaxRotate int    // 单请求最多换号次数，默认 3
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode string
	// RedisDroppedWrites 报告 Redis 异步镜像因**写队列已满**而丢弃的笔数
	// （redisstore.Upstash 有界队列；Noop/未注入时恒 0）。
	// 本地状态始终是权威来源，丢弃只影响重启恢复的镜像完整度；该计数是运维
	// 发现“Redis 长期不可用/写跟不上”的唯一量化信号。
	RedisDroppedWrites func() uint64
	SoftCooldown       time.Duration // 429/限流文案软冷却基数，默认 600s（连续触发指数退避，封顶 soft_rate_max）
	RefreshSkew        time.Duration // token 提前刷新窗口，默认 10m

	// Panel 管理面板 handler（可选；nil = 不挂载）。挂载在 /panel/ 前缀下，
	// 面板自带 Bearer 鉴权（同一 api_key）与内嵌静态资源，主路由只做转发。
	Panel http.Handler

	// Live 运行期可变配置（面板在线改 api_key / soft_rate / 脱敏开关时立即生效）。
	// nil 时回退静态字段（测试与裸用场景）。
	Live *runtime.Holder

	// PromptHold 分域提示词规则的运行期快照（可热改；nil 时回落到 PromptRules/
	// PromptMode 历史字段，测试与裸用路径零改动）。装配期由 main/config 注入，
	// 保存配置时整体 Store 新规则。
	PromptHold *prompt.Holder

	// PromptRules 各域提示词规则（键："" 默认、cn、global），键不存在时回落 ""。
	// 仅作 PromptHold 为 nil 时的静态回退（测试/历史调用方）；生产走 PromptHold。
	PromptRules map[string]prompt.Rule

	// PromptMode 历史字段：无 PromptRules 时的单一规则模式（none/replace/append/after）。
	PromptMode string
	// PromptText 历史字段：PromptMode 对应的正文。
	PromptText string

	// GlobalEnabled global realm 路由开关（config global.enabled，缺省 true）。
	// handler 侧第三道闸（与 main 注入 auth 开关、upstream 热改快照呼应）：
	// false（显式逃生门）时即便 auth realm=global 也不提供 global: 模型名
	// （modelList 不列 global 名单）。热改走 SetGlobalEnabled（内部原子值），
	// 本字段只作装配期初值。
	GlobalEnabled bool

	// RealmResolver 裸模型名的域策略解析器（config model_default_realm）。
	// nil = 默认 cn（零回归）。必须与 session 粘性闭包用同一个实例。
	RealmResolver *RealmResolver

	// Usage 逐请求用量记录器（可选；nil = 不记录）。
	// 在 recordAttempt 这一唯一汇聚点调用，因此流式/非流式、成功/失败都会计入，
	// 且与 pool 的每账号累计器同源，两条口径不会漂移。
	Usage *usage.Recorder

	// RequestLog 请求指标与脱敏 JSONL 归档（可选；nil = 不记录）。
	RequestLog *reqlog.Recorder

	// RecordClientInfo 是否在请求日志里记录调用来源（客户端 IP / User-Agent）。
	// 来自 logging.request_client_info（缺省 true）；关闭时 reqlog 事件的来源字段
	// 保持为空，归档与面板都不出现来源信息。
	RecordClientInfo bool

	// MaxInflightRequests / MaxInflightBytesMB / IngressWait 服务级入站准入
	// （见 ingress.go）：限制「同时读取 + 整包解析 + 图片校验」的请求数与字节预算。
	// 0 = 该项不限制。三者都为 0 时准入闸门完全关闭（保持旧行为）。
	MaxInflightRequests int
	MaxInflightBytesMB  int
	IngressWait         time.Duration
}

// loadLive 返回当前运行期快照；Live 为 nil 时用静态字段合成。
func (h *Handler) loadLive() runtime.Snapshot {
	if h.cfg.Live != nil {
		return h.cfg.Live.Load()
	}
	return runtime.Snapshot{
		APIKey:           h.cfg.APIKey,
		SoftCooldown:     h.cfg.SoftCooldown,
		RecordClientInfo: h.cfg.RecordClientInfo,
	}
}

// realmResolver 返回生效的域策略解析器；未注入时用默认 cn（零回归兜底，供测试/裸用）。
func (h *Handler) realmResolver() *RealmResolver {
	if h.cfg.RealmResolver != nil {
		return h.cfg.RealmResolver
	}
	return &RealmResolver{Default: RealmDefaultCN}
}

// SetGlobalEnabled 热替换 global 域路由开关（配置保存路径调用）。
func (h *Handler) SetGlobalEnabled(enabled bool) {
	if h == nil {
		return
	}
	h.globalEnabled.Store(enabled)
}

// promptRuleFor 返回本次请求（指定 realm）生效的提示词规则。
//
// 查找顺序：精确 realm（cn/global）→ "" 默认规则。两者都不存在或规则为
// none/正文空时 ok=false（调用方直接跳过组合，与历史 passthrough 行为一致）。
// 规则从 PromptHold 原子快照读取，配置热改即时生效。
func (h *Handler) promptRuleFor(realm string) (prompt.Rule, bool) {
	rules := h.promptHold.Load()
	if len(rules) == 0 {
		return prompt.Rule{}, false
	}
	r, ok := rules[realm]
	if !ok {
		r, ok = rules[""]
	}
	if !ok {
		return prompt.Rule{}, false
	}
	switch r.Mode {
	case prompt.ModeReplace, prompt.ModeAppend, prompt.ModeAfter, prompt.ModeInject:
	default:
		return prompt.Rule{}, false // none / 空 / 未知：不改写
	}
	if r.Text == "" {
		return prompt.Rule{}, false
	}
	return r, true
}

// softCooldown 返回当前生效的软冷却基数（热改优先，<=0 回退默认）。
func (h *Handler) softCooldown() time.Duration {
	if d := h.loadLive().SoftCooldown; d > 0 {
		return d
	}
	if h.cfg.SoftCooldown > 0 {
		return h.cfg.SoftCooldown
	}
	return 600 * time.Second
}

// notFoundCooldown 上游 404 的固定短冷却时长。
// 与 SoftCooldown 分流的原因：404 是上游**偶发**路径缺失，不是"本账号在限流"，
// 若共用 soft_rate（600s 起 + 指数升级），一次偶发 404 会把好账号罚 10 分钟并逐次加倍。
// 故固定 60s 防雪崩即可，不随 soft_rate 配置、也不参与软退避指数。
const notFoundCooldown = 60 * time.Second

// ServiceName 网关身份标识。经 /healthz 响应体 service 字段与 X-Service 头同时透出：
// 宿主（如 workbuddy-switch 托管网关子进程）探测同端口的旧服务/其他服务时，对方即使
// 返回 2xx 也不带本标识，宿主据此可识别"假成功"。
const ServiceName = "workbuddy_manager"

// Handler 主路由。
type Handler struct {
	cfg Config
	mux *http.ServeMux
	// ingress 服务级入站准入（读 + 解析阶段的并发/字节预算）。见 ingress.go。
	ingress *ingressLimiter
	// promptHold 提示词规则原子快照（配置可热改；总是非 nil）。
	promptHold *prompt.Holder
	// globalEnabled global 域路由开关的运行期值（config global.enabled 热改）。
	globalEnabled atomic.Bool
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 600 * time.Second // 软限流基数（连续触发按指数退避放大）
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.PromptMode == "" {
		cfg.PromptMode = prompt.ModeInject
	}
	// 提示词规则：优先用注入的热改 holder；没有时用静态字段合成一份（兼容
	// 测试/裸用路径）。两种情况热路径都只查 holder，分支唯一。
	if cfg.PromptHold == nil {
		rules := cfg.PromptRules
		if len(rules) == 0 {
			rules = map[string]prompt.Rule{
				"": {Mode: cfg.PromptMode, Text: cfg.PromptText},
			}
		}
		cfg.PromptHold = prompt.NewHolder(rules)
	}
	h := &Handler{
		cfg:        cfg,
		mux:        http.NewServeMux(),
		ingress:    newIngressLimiter(cfg.MaxInflightRequests, cfg.MaxInflightBytesMB, cfg.IngressWait),
		promptHold: cfg.PromptHold,
	}
	h.globalEnabled.Store(cfg.GlobalEnabled)
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("POST /v1/responses", h.withAuth(h.responses))
	h.mux.HandleFunc("POST /v1/messages", h.withMessagesAuth(h.messages))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	if cfg.Panel != nil {
		h.mux.Handle("/panel/", cfg.Panel) // /panel → /panel/ 由 ServeMux 自动重定向
	}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.cfg.RequestLog != nil && r.Method == http.MethodPost && isInferencePath(r.URL.Path) {
		trace := &requestTrace{id: reqlog.NewRequestID(), start: time.Now(), path: r.URL.Path}
		if h.loadLive().RecordClientInfo {
			trace.captureClientInfo(r)
		}
		r = r.WithContext(context.WithValue(r.Context(), requestTraceKey{}, trace))
		obs := &responseObserver{ResponseWriter: w}
		w.Header().Set("X-Request-Id", trace.id)
		h.cfg.RequestLog.Begin()
		defer func() {
			status := obs.status
			if status == 0 {
				status = http.StatusOK
			}
			h.cfg.RequestLog.Record(trace.event(status))
		}()
		h.mux.ServeHTTP(obs, r)
		return
	}
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !httpauth.VerifyBearer(r, h.loadLive().APIKey) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	// 用 ServableNow 判定：healthy>0 但全占满在途时 chat 会 503，探活必须同口径，
	// 否则负载均衡器会把流量持续打进无法受理的实例。
	status := http.StatusOK
	if !h.cfg.Pool.ServableNow() {
		status = http.StatusServiceUnavailable
	}
	// realm_servable 域可服务维度：不改判活语义（存在性探活保持不变），
	// 只新增 CN/global 各自可达性供双域部署运维观察（任一域不可用单独告警）。
	realmServable := map[string]bool{
		"cn":     h.cfg.Pool.ServableForRealm("cn"),
		"global": h.cfg.Pool.ServableForRealm("global"),
	}
	// 恒无鉴权（负载均衡/编排探活只需 2xx/503 语义），身份靠 service 字段 + X-Service 头双保险。
	w.Header().Set("X-Service", ServiceName)
	writeJSON(w, status, map[string]any{
		"healthy":        healthy,
		"total":          total,
		"service":        ServiceName,
		"realm_servable": realmServable,
	})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	var droppedWrites uint64
	if h.cfg.RedisDroppedWrites != nil {
		droppedWrites = h.cfg.RedisDroppedWrites()
	}
	// cost_explore 探索台账（issue #136 §5 可观测性）：累计探索事件数 + 各
	// (域, 模型) 的最近探索时刻（键 "realm|model"）。与 accounts[].model_costs
	// 行对照即可读出「探索→毕业」全链路（单一事实来源，不做双表示）。零回归只增键。
	exploreEvents, exploreLast := h.cfg.Pool.CostExploreStatus()
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":       h.cfg.Pool.List(),
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
		// realm_totals 按域分组的计数汇总（双 realm 并存时运维一眼看到各域可用性）：
		// 只新增字段，既有 total/healthy/cooling/disabled/in_flight_full 汇总键不变（零回归）。
		"realm_totals": map[string]map[string]int{
			"cn":     countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("cn")),
			"global": countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("global")),
		},
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,
		// redis_dropped_writes 异步镜像队列满导致丢弃的笔数（0 = 无丢弃）：
		// 与 redis_mode 并列，回答“镜像到底有没有跟上”。零值也显式写出。
		"redis_dropped_writes": droppedWrites,
		// ingress 读/解析阶段的准入观测（未启用限制时全零）。
		// rejected > 0 表示网关曾因并发/字节预算对客户端回 503。
		"ingress": h.ingress.Stats(),
		// model_locks 当前有未过期模型级限流的 (域, 模型) 全清单：账号池视图回答
		// 「哪些号不能用」，本键回答「哪些模型不能用、锁了几个号、还要锁多久」。
		// 与 ModelBlocked（请求失败时的单模型判定）互补；无锁时为 null。零回归只增键。
		"model_locks": h.cfg.Pool.ModelLockView(),
		// credit_floor 生效的积分保底值（0 = 关闭）。与 accounts[].credits +
		// model_costs 对照即可判定「某号为何对某模型不出票」。零值也显式写出
		// （运维口径：缺失会让人误以为没记录）。
		"credit_floor": h.cfg.Pool.CreditFloor(),
		// cost_explore 事件与 per-model 时间戳（时间值由 encoding/json 写 RFC3339）。
		"cost_explore": map[string]any{
			"events_total": exploreEvents,
			"per_model":    exploreLast,
		},
	})
}

// countsMapFrom 把 CountsDetailed 五元组打包成 /status 的域分组建模。
func countsMapFrom(total, healthy, cooling, disabled, inFlightFull int) map[string]int {
	return map[string]int{
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
	}
}

// models 返回模型列表：只读目录快照（启动预热 / 面板手动刷新写入）。
// 快照为空 → 空列表（无静态兜底——拉不出目录时假名单只会让客户端选到 11102 的模型）。
// 本端点**不触发上游探测**：客户端随时可拉，零上游代价。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(),
	})
}

// fmtCreditsPrefix 从上游 credits 原文提取倍率并格式化为 "[x0.05 credit]"。
// 上游格式不统一："x0.05 credits" / "x0.29" / "x0.00 credits" 等，
// 统一提取 x数字 部分，去 "credits" 后缀。
func fmtCreditsPrefix(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, "credits")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return "[" + s + " credit]"
}

// applyModelInfoFields 把上游模型对象全字段（ModelInfo）按「空值省略」写出规则
// 合入 /v1/models 条目：name/description/credits/tags/vendor/能力旗标/
// max_allowed_size/reasoning_effort/reasoning_summary。CN 动态分支与 global
// 探测命中分支共用（两域模型对象同构），保证输出字段集一致。
// 不覆盖 id/object/created/owned_by 及调用方先前写好的基础字段；上游未下发的
// 字段（零值）整体省略——不编造。
func applyModelInfoFields(entry map[string]any, mi upstream.ModelInfo) map[string]any {
	if mi.Name != "" {
		entry["name"] = mi.Name
	}
	if mi.Description != "" {
		// 积分倍率前缀：从 "x0.05 credits" / "x0.29" 等格式提取纯数字，
		// 统一为 "[x0.05 credit]" 前缀拼入 description，方便下游面板直接展示。
		if mi.Credits != "" {
			entry["description"] = fmtCreditsPrefix(mi.Credits) + " " + mi.Description
		} else {
			entry["description"] = mi.Description // descriptionZh 中文描述
		}
	}
	if mi.Credits != "" {
		entry["credits"] = mi.Credits // 积分倍率原文（如 "x0.05"），仅展示
	}
	if len(mi.Tags) > 0 {
		entry["tags"] = mi.Tags
	}
	if mi.Vendor != "" {
		entry["vendor"] = mi.Vendor
	}
	if mi.IsDefault {
		entry["is_default"] = true
	}
	if mi.SupportsImages {
		entry["supports_images"] = true // 多模态能力透出
	}
	if mi.SupportsReasoning {
		entry["supports_reasoning"] = true
		if mi.CanDisableThinking {
			entry["can_disable_thinking"] = true
		}
	}
	if mi.SupportsToolCall {
		entry["supports_tool_call"] = true
	}
	if mi.OnlyReasoning {
		entry["only_reasoning"] = true
	}
	if mi.MaxAllowedSize > 0 {
		entry["max_allowed_size"] = mi.MaxAllowedSize
	}
	if mi.ReasoningEffort != "" {
		entry["reasoning_effort"] = mi.ReasoningEffort
	}
	if mi.ReasoningSummary != "" {
		entry["reasoning_summary"] = mi.ReasoningSummary
	}
	return entry
}

// modelList 模型列表：CN 模型输出统一加 "cn:" 前缀（gateway 路由协议，与 resolveModel
// 对称）；global.enabled=true 时追加 global: 前缀的国际版名单。
// 纯动态 + 缓存制：只读目录快照（启动预热 / 面板手动刷新写入），
// 快照为空 → 该域空列表；本方法**绝不触发上游探测**（客户端请求路径零上游调用）。
func (h *Handler) modelList() []map[string]any {
	out := make([]map[string]any, 0)
	if h.cfg.Upstream == nil {
		return out
	}
	for _, mi := range h.cfg.Upstream.Catalog("cn").Infos {
		entry := map[string]any{
			"id":       "cn:" + mi.ID,
			"object":   "model",
			"created":  1753600000,
			"owned_by": "workbuddy",
		}
		// context_length / max_output_tokens 四级查找（upstream.model_catalog）：
		// 上游动态值（maxInputTokens/maxOutputTokens）权威 → 静态种子表 →
		// model.json 本地缓存 → models.dev 按需拉取（异步不阻塞本次响应，拉到后
		// 写 model.json 供下次命中）→ 1M 兜底 / max_output_tokens 省略。
		// 上游零值不再透出假 131072（误导 Codex/ZCode 等按 context_length 提前
		// 截断、白白丢上下文）。
		entry["context_length"] = upstream.ContextWindowListingV4(mi.ID, mi.ContextWindow, h.cfg.Upstream.ShortClient())
		if mo, ok := upstream.MaxOutputTokensListingV4(mi.ID, mi.MaxTokens, h.cfg.Upstream.ShortClient()); ok {
			entry["max_output_tokens"] = mo
		}
		// 上游模型对象全字段透出（name/描述/标签/倍率/能力旗标等，空值省略）。
		entry = applyModelInfoFields(entry, mi)
		// effort 能力透出——远端 supportedEfforts 权威，缺失落到 CN 静态兜底表
		// （客户端可发现档位，不再盲传）。无档位 → 省略字段。
		if efforts, def := upstream.EffortListing("cn", mi.ID, mi.Efforts, mi.DefaultEffort); efforts != nil {
			entry["reasoning_supported_efforts"] = efforts
			if def != "" {
				entry["reasoning_default_effort"] = def
			}
		}
		out = append(out, entry)
	}
	// global 模型名单：仅 GlobalEnabled=true 时列出（逃生门）。
	// 名单 = global 域目录快照（启动预热 / 面板手动刷新写入），空 → 该域空列表。
	if h.globalEnabled.Load() {
		// global 域 effort 能力三级查找：探测下发桶（权威）→ 静态兜底表 → 省略。
		// 快照里的 ModelInfo 已带 supportedEfforts/defaultEffort，直接透出。
		globalEfforts, globalDefaults := h.cfg.Upstream.GlobalEffortSnapshot()
		for _, mi := range h.cfg.Upstream.Catalog("global").Infos {
			id := mi.ID
			entry := map[string]any{
				"id":       "global:" + id,
				"object":   "model",
				"created":  1753600000,
				"owned_by": "workbuddy",
			}
			// context_length / max_output_tokens 四级查找（与 CN 动态分支同口径）。
			// 快照条目自带上游动态值（maxInputTokens/maxOutputTokens），直接透出。
			entry = applyModelInfoFields(entry, mi)
			entry["context_length"] = upstream.ContextWindowListingV4(id, mi.ContextWindow, h.cfg.Upstream.ShortClient())
			if mo, ok := upstream.MaxOutputTokensListingV4(id, mi.MaxTokens, h.cfg.Upstream.ShortClient()); ok {
				entry["max_output_tokens"] = mo
			}
			if efforts, def := upstream.EffortListing("global", id, globalEfforts[id], globalDefaults[id]); efforts != nil {
				entry["reasoning_supported_efforts"] = efforts
				if def != "" {
					entry["reasoning_default_effort"] = def
				}
			}
			out = append(out, entry)
		}
	}
	return out
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	h.inference(w, r, protocol.Chat)
}

// inference is the single account-routing, retry and accounting path for all
// protocols. Only input decoding and output encoding depend on the client API.
func (h *Handler) inference(w http.ResponseWriter, r *http.Request, kind protocol.Kind) {
	out := inferenceOutput{kind: kind}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	r = r.WithContext(ctx)
	// 客户端 IP 提取（按请求传递到 ChatStream，不透传时 upstream 侧忽略）；
	// 消除早年共享字段方案的并发交叉污染（issue：ClientIP 竞态）。
	clientIP := upstream.ExtractClientIP(r)
	// 服务级准入（ingress.go）：io.ReadAll（最多 32 MiB）+ 整包解析 + 图片校验
	// 是网关最贵的一段，且发生在账号租约之前——没有这道闸门时其并发不受任何约束。
	// 名额只覆盖到这里为止：解析完成后立刻释放（长 SSE 流不占名额）。
	release, admitted := h.ingress.Acquire(ctx, r.ContentLength)
	if !admitted {
		h.ingress.rejectLog()
		out.ErrorHint(w, http.StatusServiceUnavailable, "server_busy",
			"gateway is busy ingesting requests, please retry", upstream.NoHealthyAccountHint())
		return
	}
	// 读 + 解析期间持名额；defer 兜住所有提前返回路径（release 幂等）。
	defer release()

	// Bound the original client body before decoding or translating it.
	r.Body = http.MaxBytesReader(w, r.Body, forwarding.MaxRequestBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		status := http.StatusBadRequest
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			status = http.StatusRequestEntityTooLarge
		}
		out.Error(w, status, "invalid_request", "read body: "+err.Error())
		return
	}
	request, err := protocol.DecodeWithOptions(kind, body, protocol.DecodeOptions{
		ReplayThinking: h.cfg.Upstream.ReplayThinkingNow(),
	})
	if err == nil {
		err = request.Chat.ResolveHeaders(r.Header)
	}
	// 读取 + 解析 + 图片校验已完成：立刻释放准入名额（后续上游流式转发
	// 可能是分钟级，不能占用入站名额）。release 幂等，defer 仍会兜底。
	release()
	if err != nil {
		out.Error(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	peek := request.Chat
	out.request = request
	// 标记还原（internal/upstream/dsml.go）的工具名名单：客户端声明过哪些工具。
	// 名单来自「本请求声明的 tools」与「会话历史里出现过的工具名」两个来源
	//（tools 声明在中转环节丢失是泄漏常态，只认前者会让还原层在最需要它的场景
	// 下失效）；取自已解析文档（跨协议入口是翻译后的 Chat 形态，与本层同源），
	// 在提示词/档位改写之前读——那些改写不动 tools / messages[].tool_calls 子树。
	// 名单为空时还原层走弱判定（块结构完美 + 工具名形如标识符）。
	markupAllow := upstream.ToolNameAllowlistDoc(peek.Object)
	// 非流式响应在本地聚合后一次性写出：加写 deadline，防客户端保持连接却不读
	// 时长期占住 handler／响应内存／账号租约（SSE 已有逐帧 deadline，不重复包装）。
	if !peek.Stream {
		w = newWriteDeadlineWriter(w, nonStreamWriteTimeout)
	}
	// body 是入站原始字节，仅作为指纹层哨兵预检的来源（不再用于出站序列化：
	// 出站统一从已解析文档构造，见下 upstream.ChatInput）。当字节与文档不同源时
	// 不能拿它做预检，否则可能漏掉只在文档变换后才出现的指纹。
	sentinelRaw := body
	if kind != protocol.Chat || request.Mutated {
		// 跨协议：body 是 Responses/Anthropic 原始字节，与 Chat 形态文档不同源；
		// Mutated：文档已被工具图片抬升改写。两种情况下哨兵扫描对象不可靠，
		// 交给指纹层全量遍历（语义不变，仅少一次廉价预扫）。
		sentinelRaw = nil
	}

	// realm 前缀解析（D6）：model 名可能带 "[realm:]" 前缀。剥出 realm + bareModel，
	// bareModel 用于选号/粘性/出站 body 重写（前缀是网关侧路由协议，上游只认裸名）。
	// 裸名按 config model_default_realm 策略（cn/global/auto/auto:global,cn 系列）；
	// resolver 为 nil 时回落 cn（零回归）。
	candidateRealms, bareModel := h.realmResolver().CandidateRealms(peek.Model)
	if strings.TrimSpace(bareModel) == "" {
		out.Error(w, http.StatusBadRequest, "invalid_model", "model realm is disabled or model is empty")
		return
	}
	// 显式 global: 前缀在 global 禁用时必须报错（非静默跨域）
	if len(candidateRealms) == 1 && candidateRealms[0] == RealmDefaultGlobal && !h.globalEnabled.Load() {
		out.Error(w, http.StatusBadRequest, "invalid_model", "model realm is disabled or model is empty")
		return
	}
	if !h.globalEnabled.Load() {
		filtered := make([]string, 0, len(candidateRealms))
		for _, cr := range candidateRealms {
			if cr != RealmDefaultGlobal {
				filtered = append(filtered, cr)
			}
		}
		candidateRealms = filtered
	}
	if len(candidateRealms) == 0 {
		out.Error(w, http.StatusBadRequest, "invalid_model", "model realm is disabled or model is empty")
		return
	}
	// 请求级统计：出口即打一行表格日志（任何路径都会走到，**含门禁 404**）。
	st := newChatStat(time.Now(), peek.Model, peek.Stream)
	// 跨协议入口被忽略的客户端字段（如 Codex 的 include/reasoning 历史/客户端遥测）：
	// 只写请求归档，用于回答“它发了什么被我们丢了”。
	st.dropped = droppedFields(request.Dropped)
	st.thinking = request.Thinking
	if tr := requestTraceFrom(r); tr != nil {
		tr.stat = st
		// 来源在 ServeHTTP 入口采集（此时才知道开关与请求头），此处转交给统计对象，
		// 让 stdout 流水行与归档事件共用同一份来源值，两处不会漂移。
		st.clientIP, st.userAgent = tr.clientIP, tr.userAgent
	}
	defer st.done()

	// 模型目录门禁（modelcheck.go）：快照可用时，不存在的模型在选号之前直接 404——
	// 一个错写的模型名此前会逐个账号去上游撞 11102，每撞一次就往「模型锁池」里
	// 写一条 6h 起的模型级负缓存（Hits 达 4 次后 24h），纯属自伤。
	// 命中时顺便重排候选域（global-only 模型不再先拿 CN 号撞一次）与纠正大小写。
	// 快照为空（未刷新过）→ fail-open，维持既有行为（不给客户端请求探测上游）。
	var catalogRejected bool
	candidateRealms, bareModel, catalogRejected = h.checkModel(candidateRealms, bareModel)
	if catalogRejected {
		st.status = http.StatusNotFound
		st.outcome = reqlog.OutcomeHTTPError
		out.ErrorHint(w, http.StatusNotFound, modelNotFoundCode,
			"model not found: the gateway model catalog does not contain this model",
			modelNotFoundHint(bareModel, candidateRealms))
		return
	}
	primaryRealm := candidateRealms[0]
	modelRate := ""
	if h.cfg.Upstream != nil {
		modelRate = h.cfg.Upstream.ModelRate(primaryRealm, bareModel)
	}

	tried := map[string]bool{}
	var lastErr error
	// modelBlock 记录本次是否因「模型级冷却」而选不到号（下方 acct==nil 分支填充）。
	// 默认零值 Blocked=false = 按既有的"没有可用账号"口径报错。
	var modelBlock pool.ModelBlockStatus

	// 会话粘性：会话键按三级回退提取（75c0a78 断链后恢复）：
	//   1. conversation 维度键：peek.ConversationID（body 顶层/metadata 的
	//      conversation_id/conversationId + X-Conversation-ID 头，forwarding.Parse
	//      已解析校验）；
	//   2. 客户端会话头：session.HeaderSessionKey（Claude Code / Codex / OpenCode /
	//      pi 等客户端的自定义会话头，带来源命名空间前缀）；
	//   3. body 会话键与派生回退：session.BodyKey（非标准 JSON 键位链 →
	//      prompt_cache_key → 内容派生键，一次解析完成；带 user_id 的请求不派生
	//      ——P1-anti-monopoly 契约）。
	// 提取与粘性开关解耦（issue #35 侧）：关闭粘性时会话头族的聚合主键仍按
	// 会话级（RequestIDForKey(sessKey)），不悄悄退化成轮级。
	//
	// 调用方主体命名空间：API key 是唯一可区分的主体边界。未配置 key 时
	// 没有主体边界可言，principalID 留空（不编造隔离，且粘性键跨升级保持稳定）。
	principalID := ""
	if key := h.loadLive().APIKey; key != "" {
		sum := sha256.Sum256([]byte(key))
		principalID = hex.EncodeToString(sum[:16])
	}
	// convKey retains legacy sticky-key semantics. Optional outbound identity uses
	// its own namespaced material below, not a promotion of this coarse key.
	convKey := ""
	if peek.ConversationID != "" {
		convKey = peek.ConversationID
	} else if v := session.HeaderSessionKey(r.Header); v != "" {
		convKey = v
	} else {
		convKey = bodySessionKey(request, body)
	}
	// isDerivedKey 精确判定派生键形态（"d-" + 32 hex，与 deriveKey 输出严格
	// 同宽）：prompt_cache_key 是裸值入键，客户端可能碰巧传 "d-" 开头的值，
	// 宽松前缀判定会把这类显式键误入救场/记录路径。显式键不会漂移，两路都
	// 应排除。
	isDerivedKey := len(convKey) == len(session.DerivedKeyPrefix)+32 &&
		strings.HasPrefix(convKey, session.DerivedKeyPrefix)
	// 指纹救场（仅派生键会话）：派生键的正确性依赖 system+首条 user 每轮恒定，
	// system 带时间戳/动态注入的客户端每轮键都变 → 粘性永久失效。窗口匹配
	// （user/assistant 历史，排除 system）对这类漂移免疫：用消息指纹找回该
	// 会话上一跳使用的账号，成功后 Bind 到**本请求的派生键**（单次搭便车——
	// 不借用候选键，下一轮自己的键已有绑定，救场不再参与）。
	//   - 仅当派生键当前无有效绑定时触发。探测必须用只读 PeekForModel——
	//     ResolveForModel 未命中会走慢路径分配新绑定，把救场结果覆盖掉。
	//   - 候选按 principal+model 收窄；误配代价是单跳搭错车（TTL 内自愈），
	//     非正确性问题。
	fpRescue := false
	sessKey := ""
	stickyUID := ""
	if convKey != "" && isDerivedKey && h.cfg.Session != nil && h.cfg.Session.FingerprintEnabled() {
		probeKey := primaryRealm + ":" + bareModel + ":" + convKey
		if principalID != "" {
			probeKey = principalID + ":" + probeKey
		}
		if _, ok := h.cfg.Session.PeekForModel(probeKey, peek.Model); !ok {
			if ruid, rok := h.cfg.Session.RescueDerivedKey(body, principalID, bareModel, peek.Model); rok {
				stickyUID = ruid
				fpRescue = true
				log.Printf("[session] 指纹救场命中 uid=%s conv=%s…", logfmt.UID8(ruid), convKey[:min(16, len(convKey))])
			}
		}
	}
	if convKey != "" {
		// 在候选域中按优先级检查既有会话绑定（避免多轮对话因自动轮转发生跨域漂移）。
		// 救场命中时跳过：ResolveForModel 的慢路径会给本键分配新绑定，覆盖救场结果。
		if !fpRescue && h.cfg.Session != nil {
			for _, cr := range candidateRealms {
				k := cr + ":" + bareModel + ":" + convKey
				if principalID != "" {
					k = principalID + ":" + k
				}
				if uid, ok := h.cfg.Session.ResolveForModel(k, peek.Model); ok {
					stickyUID = uid
					sessKey = k
					break
				}
			}
		}
		if sessKey == "" {
			sessKey = primaryRealm + ":" + bareModel + ":" + convKey
			if principalID != "" {
				sessKey = principalID + ":" + sessKey
			}
		}
	}

	// 请求形态（image_url part）：从已解析对象扫描（跨协议已转换为 Chat 形态）。
	// 旧实现按原始 body 反序列化，同一请求里混有字符串 content 消息时会整体失败，
	// 导致 gateway_hint 的「请求确实带图」判定漏判。
	reqHasImage := media.HasImages(request.Chat.Object)

	// 在途租约：成功选中即占名额；函数出口（含成功 return 与 panic）统一释放。
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	// unbindSticky 解绑当前会话粘性号（stickyUID 非空时）。供「粘性号不可用/被抢」与 fail 共用。
	// 幂等：stickyUID 已空则空操作；不会误解绑其他轮的绑定。仅当 Session != nil 时 stickyUID 才会非空。
	unbindSticky := func() {
		if stickyUID != "" {
			h.cfg.Session.Unbind(sessKey)
			stickyUID = ""
		}
	}
	// fail 在轮转失败分支统一：释放租约 + 若失败号正是粘性号则解绑（下次请求重新分配）。
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			unbindSticky()
		}
	}
	// ttfb 首 token 等待（仅流式有观测；非流式传 0 = 无观测，速率不扣减）。
	// 显式入参而不是读 st.ttfb：后者在流式分支里是**调用之后**才赋值的，
	// 靠顺序传递会让将来重排代码时静默把速率算回旧的错口径。
	recordAttempt := func(uid string, delta pool.TokenUsageDelta, credit float64, hasCredit bool, started time.Time, ttfb time.Duration) {
		st.attempts++
		if delta.HasPromptTokens {
			st.promptTokens = delta.PromptTokens
		}
		if delta.HasCompletionTokens {
			st.completionTokens = delta.CompletionTokens
		}
		if delta.HasTotalTokens {
			st.totalTokens = delta.TotalTokens
		} else if delta.HasPromptTokens || delta.HasCompletionTokens {
			st.totalTokens = st.promptTokens + st.completionTokens
		}
		delta.Model = bareModel
		if delta.Model == "" {
			delta.Model = peek.Model
		}
		latency := time.Since(started)
		latencyMs := latency.Milliseconds()
		if latencyMs < 1 {
			latencyMs = 1
		}
		delta.HasLatencyMs = true
		delta.LatencyMs = latencyMs

		// HasCompletionTokens 是「本次有没有 token 观测」的独立判断，与速率怎么算
		// 无关，所以留在调用点；速率本身的边界（TTFB 缺失/超界）收在 tokensPerSecond。
		if delta.HasCompletionTokens {
			if tps, ok := tokensPerSecond(delta.CompletionTokens,
				time.Duration(latencyMs)*time.Millisecond, ttfb); ok {
				delta.HasTokensPerSecond = true
				delta.TokensPerSecond = tps
			}
		}
		h.cfg.Pool.RecordTokenUsage(uid, delta)

		// 用量时序记录。ok 以「上游是否给了 usage」判定：空 delta 意味着这次尝试
		// 没拿到任何 token 统计（传输错误 / >=400 / 解析失败），计为失败尝试。
		// 失败也计入请求数——否则重试放大在「用量」视图里看不见。
		if h.cfg.Usage != nil {
			realm := "cn"
			if a, ok := h.cfg.Pool.Status(uid); ok && a.Realm != "" {
				realm = a.Realm
			}
			actualModelRate := modelRate
			if h.cfg.Upstream != nil {
				actualModelRate = h.cfg.Upstream.ModelRate(realm, bareModel)
			}
			h.cfg.Usage.Add(time.Now(), realm, uid, delta.Model, usage.Delta{
				PromptTokens:     delta.PromptTokens,
				HasPromptTokens:  delta.HasPromptTokens,
				CompletionTokens: delta.CompletionTokens,
				HasCompletion:    delta.HasCompletionTokens,
				TotalTokens:      delta.TotalTokens,
				HasTotal:         delta.HasTotalTokens,
				Credit:           credit,
				HasCredit:        hasCredit,
				HasCacheTokens:   st.hasCache,
				CacheHitTokens:   st.cacheHit,
				CacheMissTokens:  st.cacheMiss,
				ModelRate:        actualModelRate,
				LatencyMs:        delta.LatencyMs,
				HasLatency:       delta.HasLatencyMs,
				TokensPerSecond:  delta.TokensPerSecond,
				HasTPS:           delta.HasTokensPerSecond,
			}, delta.HasTotalTokens || delta.HasCompletionTokens || delta.HasPromptTokens)
		}
	}

	// 出站基线：模型名统一由 upstream 从文档设置（realm 前缀由 bareModel 剥除），
	// 不再对字节做一次 rewriteModel 往返（旧实现：解析 + 重新序列化）。
	baseObj := peek.Object
	// Responses 回显的基线必须保持原样：失败 attempt 注入/钳位的档位不能带到
	// 下一个域。每次出站前仅复制 reasoning 子树，不复制整个入参/消息历史。
	baseReasoning, hasBaseReasoning := request.Source["reasoning"]

	// 会话头族（issue #35）：后台按 X-Conversation-Request-ID（对话轮级）聚合请求，
	// 官方客户端一次 user send 内所有 tool call/重试/换号复用同一个 ID。此处**轮转
	// 循环外**生成一次，循环内每次出站原样复用 → 换号/重试/降级全部同 ID，后台不再
	// 碎片化（此前网关一个都不发，上游按 HTTP 请求逐条记账，同一对话几十上百个
	// RequestID）。
	//   - conversationID：body 提取（透传客户端原值；缺省空串 = 不伪造，除非
	//     upstream.derive_conversation_id 开启——见下方派生分支）；
	//   - conversationRequestID：入站 X-Conversation-Request-ID 透传优先，否则按
	//     粘性 key 进程内稳定生成；粘性 key 也空时走轮级兜底（TurnKey/TurnRequestID），
	//     无 user 消息时退化成本请求级随机——轮转内捕获一次即共享；
	//   - messageID 在 ChatHeaders 内每条消息生成（消息级独立，无需外部可见）。
	//
	// Optional outbound identity is a cache/routing experiment, not proof of an
	// upstream stateful cache. Preserve explicit IDs; otherwise use principal-
	// scoped client session material or normalized opening content. Identical
	// openings can still collide, so clients should supply a real session key.
	conversationID := peek.ConversationID
	deriveConversationID := false
	if conversationID == "" && h.cfg.Upstream != nil && h.cfg.Upstream.DeriveConversationIDsNow() && convKey != "" {
		identityKey := session.HeaderSessionKey(r.Header)
		if identityKey == "" {
			source := request.Source
			if source == nil {
				// Native Chat has no separate Source. Hoisting only changes messages,
				// so its explicit body session fields remain available in Chat.Object.
				source = request.Chat.Object
			}
			identityKey = session.ExplicitBodyKeyObject(source, request.Chat.PromptCacheKey)
		}
		if identityKey != "" {
			identityKey = "explicit:" + identityKey
		} else {
			// convKey != "" above retains the ordinary user_id suppression gate.
			// Normalized Chat includes Messages.system and exact first-user content.
			identityKey = session.ContentConversationKey(request.Chat.Object)
		}
		conversationID = session.DerivedConversationID(identityKey, principalID)
		deriveConversationID = conversationID != ""
	}
	chatMeta := upstream.ChatMeta{
		PrincipalID:           principalID,
		ConversationID:        conversationID,
		ConversationRequestID: r.Header.Get("X-Conversation-Request-ID"),
		RootRequestID:         r.Header.Get("X-Root-Request-ID"),
		ParentConversationID:  r.Header.Get("X-Parent-Conversation-ID"),
		AgentType:             r.Header.Get("X-Agent-Type"),
		AgentPurpose:          r.Header.Get("X-Agent-Purpose"),
		ExpertID:              r.Header.Get("X-Expert-Id"),
		ExpertTeamTask:        r.Header.Get("X-Expert-Team-Task"),
		TraceID:               r.Header.Get("X-Trace-ID"),
		Traceparent:           r.Header.Get("traceparent"),
		B3TraceID:             r.Header.Get("X-B3-TraceId"),
		ACPConnectionID:       r.Header.Get("acp-connection-id"),
	}
	if deriveConversationID {
		// 归档可见性：归档里能回答“这次出站的会话身份是客户端给的还是网关派生的”
		//（A/B 与排障都要）。不进 rewritten 列表：那是“改写生成参数”的短清单，
		// 派生会话 id 是每个请求都会发生的事实，会把它挤满。
		st.conversationDerived = true
	}
	if chatMeta.ConversationRequestID == "" {
		chatMeta.ConversationRequestID = session.NewMessageID()
	}

	for i := 0; i < h.cfg.MaxRotate; i++ {
		// 选号：粘性号优先（PickByUIDForModel 已校验该模型可用性 + 在途未满），否则普通轮换。
		var acct *auth.Auth
		if stickyUID != "" {
			acct = h.cfg.Pool.PickByUIDForModel(stickyUID, bareModel)
			if acct == nil || !containsRealm(candidateRealms, acct.Realm()) {
				// 粘性号在当前模型不可用（冷却/占满/该模型被 6004 限额）或 realm 不符 → 解绑，
				// 本次回落普通轮换。
				unbindSticky()
				acct = nil
			}
		}
		if acct == nil {
			// 模型感知 + realm 感知选号：按候选域优先级遍历选号（主备轮退）
			for _, cr := range candidateRealms {
				acct = h.cfg.Pool.PickExcludingForRealm(tried, bareModel, cr)
				if acct != nil {
					break
				}
			}
		}
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			// 记下"是不是模型级阻塞"。选号返回 nil 有两种完全不同的成因：
			//   (a) 池子真的没有可用号（账号级冷却/在途占满/积分保底）；
			//   (b) 号都在，但每个号都对这个模型处于模型级冷却（11102/6004）。
			// 两者此前都报 no_healthy_account，导致「模型不可用」被读成「号全挂了」。
			// 在这里取一次快照，供下方错误构造区分（issue #102 附带发现 1）。
			modelBlock = h.cfg.Pool.ModelBlocked(bareModel)
			break
		}
		st.uid = acct.UID
		// 同步昵称：请求流水行只写 uid8 时无法直观看是哪一号，昵称随本次选号带入日志行。
		st.nick = acct.NicknameValue()
		tried[acct.UID] = true

		// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
		if !h.cfg.Pool.Acquire(acct.UID) {
			// 若被抢的正是粘性号，立即解绑并回落普通轮换，避免下一轮仍撞同一个
			// 满载粘性号再浪费一次 PickByUID 往返（语义与 fail()/PickByUID-nil 的解绑一致）。
			if stickyUID != "" && acct.UID == stickyUID {
				unbindSticky()
			}
			if !rotateBackoff(i, r.Context()) {
				// 客户端已断连：换号重试无意义，终止轮转走末端错误透传。
				break
			}
			continue // 最后一个名额被并发抢走 → 换号
		}
		heldUID = acct.UID

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.Disable(acct.UID, "refresh session dead")
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				if !rotateBackoff(i, r.Context()) {
					break // ctx 取消：终止轮转（refresh 失败换号退避）
				}
				continue
			}
			if err := acct.SaveAtomic(); err != nil {
				// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露
				log.Printf("ERR: [server] chat refresh acct=%s: save auth failed: %v", logfmt.Label(acct.UID, acct.NicknameValue()), err)
			}
		}

		// 系统提示词组合：按实际选中账号所属域应用（同一网关对 CN 与 Global 可配不同规则）。
		// 组合在**深拷贝**上进行（baseObj 在整个轮转循环里只读），避免一次「字节解析 +
		// 重新序列化」的往返；未启用规则时零拷贝直接复用基线对象。
		//
		// 正文是官方渲染产物的逐字拷贝（无运行期变量），直接取用即可。
		attemptObj := baseObj
		attemptRaw := sentinelRaw
		// attemptCopied 标记 attemptObj 已是 baseObj 的私有副本：提示词组合与档位归一
		// 各自都可能需要写时拷贝，不能各拷一次（大 body 上是双倍分配）。
		attemptCopied := false
		if rule, ok := h.promptRuleFor(acct.Realm()); ok {
			attemptObj = jsondoc.CopyObject(baseObj)
			attemptCopied = true
			text := rule.EffectiveText()
			prompt.ComposeObject(attemptObj, text, rule.Mode)
			// 出站提示词指纹（仅内部规则生效时）：11128 复盘要能回答"这次发出去的是什么形状"。
			// 正文不入档（体积/隐私），但指纹可复现——同 mode/source/sha256 即同一份内容。
			st.promptMode = rule.Mode
			st.promptPreset = rule.Source
			st.promptSHA = prompt.TextSHA(text)
			st.promptChars = utf8.RuneCountInString(text)
			// 组合后的文档与入站字节不同源：哨兵预检不再适用于本对象（见上）。
			attemptRaw = nil
		}
		attempt := *peek
		attempt.Object = attemptObj
		attempt.Model = bareModel
		if kind == protocol.Responses {
			if hasBaseReasoning {
				request.Source["reasoning"] = jsondoc.Copy(baseReasoning)
			} else {
				delete(request.Source, "reasoning")
			}
		}

		// 档位归一（reasoning effort 钳位）：放在**按实际选中账号所属域**的位置，
		// 因为同一模型名在 CN 与 global 的合法档位可以不同（deepseek-v4.1-flash：
		// CN [low high max] / global [high]），笼统钳在解析层会拿错域的集合。
		// 上游对越界档位回 400 code=11133（账号与 body 无关，换号无用），所以只能在
		// 发送前归一：回退目标是本地默认档覆盖（upstream.effort_default_overrides，
		// 见 effort_override.go），未配置该模型时维持既有「≤请求档位的最高支持档」；
		// 没有归一依据（未知模型/未知档位）一律透传，交给上游裁决。
		//
		// 与提示词组合同样在所有 attempt 共享的 baseObj 上做写时拷贝：改写后的文档
		// 与入站字节不再同源，哨兵预检必须失效（attemptRaw=nil）。
		if effortKey, from, to := h.cfg.Upstream.RequestEffortClamp(attemptObj, acct.Realm(), bareModel); from != "" && from != to {
			if !attemptCopied {
				attemptObj = jsondoc.CopyObject(baseObj)
				attempt.Object = attemptObj
				attemptCopied = true
			}
			upstream.SetRequestEffort(attemptObj, effortKey, to)
			attemptRaw = nil
			// 客户端可见性：近似变换必须能回答「我选的档位到底被判成了什么」。写入归档的
			// **改写** 列表（不是 dropped：这里是换了个值执行了，不是丢弃），面板与搜索
			// 都能按 key:原值→实际值 定位。
			st.noteRewrite("reasoning.effort:" + from + "→" + to)
			// Responses 响应体会回显请求侧 reasoning（responseEnvelope）：回显钳位后的
			// 实际生效值，避免「回显一个没被执行的档位」。跨 attempt 重试时以最后一次
			// 实际发出的域为准。
			if m, ok := request.Source["reasoning"].(map[string]any); ok {
				m["effort"] = to
			}
		}
		// 本地默认思考档注入（upstream.effort_default_overrides）：客户端一个档位都
		// 没带、也没显式关思考时，补上本地配置的默认档（空表/未命中 = 零行为变化，
		// 与上游默认行为逐字一致）。这是**显式可见**的近似变换：写入归档 rewritten
		// 列（面板与搜索都看得到），Responses 桥回显实际生效档。
		//
		// 归一在前、注入在后，两者互斥（上面只在「客户端表态且值需改写」时触发；
		// 这里只在客户端未表态时触发），不可能同时对同一字段写两次。
		if effortKey, to := h.cfg.Upstream.RequestEffortDefault(attemptObj, acct.Realm(), bareModel); to != "" {
			if !attemptCopied {
				attemptObj = jsondoc.CopyObject(baseObj)
				attempt.Object = attemptObj
				attemptCopied = true
			}
			upstream.SetRequestEffort(attemptObj, effortKey, to)
			attemptRaw = nil // 文档与入站字节不同源，哨兵预检必须失效（同归一分支）
			st.noteRewrite("reasoning.effort:默认→" + to)
			if m, ok := request.Source["reasoning"].(map[string]any); ok {
				m["effort"] = to
			} else if kind == protocol.Responses {
				// 入参本无 reasoning：仅 Responses 桥回显生效档（其余协议不回显请求参数，
				// 凭空加键会改变桥自身的入参快照语义）。
				request.Source["reasoning"] = map[string]any{"effort": to}
			}
		}
		// 客户端 IP 按请求传递（PassthroughIP 开启时注入；消除共享字段竞态）。
		attemptStarted := time.Now()
		st.noteUpstreamError("", "") // each actual attempt owns its diagnostics
		rc, status, respBody, terr := h.cfg.Upstream.ChatStreamInput(r.Context(), acct,
			upstream.ChatInput{Req: &attempt, Raw: attemptRaw, Model: bareModel}, clientIP, chatMeta)
		// 分类信封一次成型：upstream 已在错误路径返回 *upstream.Error（Kind +
		// Retry-After 头解析）。传输层错误（非 *Error）走抖动换号分支；防御分支
		// （terr 为 nil 但 status>=400，如 ErrNone 兜底）回落本地 Classify，双保险。
		var uerr *upstream.Error
		if errors.As(terr, &uerr) {
			status = uerr.Status
		}
		if uerr == nil && terr != nil {
			// 上游超时 / 停滞：**不换号、不罚号**。
			//
			// 超时不是账号的问题：同一份请求换到别的号，撞上的是同一个慢上游，
			// 只会把客户端拖到 MaxRotate × header_timeout（部署值 600s 时最坏
			// 约半小时），期间还给一串健康号喂连败计数。此前全仓没有任何超时
			// 识别，超时和"网络抖动"共用同一条换号路径。
			//
			// 判定三态：net.Error.Timeout()（ResponseHeaderTimeout / Client.Timeout）、
			// 显式 deadline（DeadlineExceeded / os.ErrDeadlineExceeded）、以及
			// **客户端仍在但 ctx 被取消**——那只能是我们自己的空闲看门狗掐的流，
			// 也就是上游停滞。客户端主动断连时 r.Context() 已取消，走下面的抖动分支。
			if isUpstreamTimeout(terr, r.Context().Err() != nil) {
				recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
				st.status = http.StatusServiceUnavailable
				lastErr = fmt.Errorf("%w: %v", errUpstreamTimeout, terr)
				log.Printf("WARN: [server] upstream timeout acct=%s: %v (rotation stopped, account not penalized)",
					logfmt.Label(acct.UID, acct.NicknameValue()), terr)
				break
			}
			// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
			// 连败兜底（issue #114）：喂连败计数——连不上上游是「不知道原因的失败」，
			// 连败 N 次临时出池，单次/偶发不罚（NoteFailures 内部达阈才动作）。
			// 上游 client 已打 transport error 日志。
			// Acceptance is unknown after a transport error; never replay generation.
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
			st.status = http.StatusBadGateway
			st.outcome = reqlog.OutcomeHTTPError
			if r.Context().Err() != nil {
				st.outcome = reqlog.OutcomeInterrupted
				return
			}
			out.Error(w, st.status, "upstream_transport", "upstream transport failed; request was not replayed")
			return
		}
		if status >= 400 {
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
			st.status = status
			var kind upstream.ErrKind
			if uerr != nil {
				kind = uerr.Kind
			} else {
				kind = upstream.Classify(status, string(respBody))
				uerr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			}
			// 上游业务码入观测（归档 upstream_kind / upstream_code）：回答“上游到底回了
			// 什么码”（队列 6020/6021/6022、11102、6004 …）。此前这些只存在于上游层的
			// stdout 日志（200 字符截断），归档层完全看不到——于是队列这类新机制的
			// 出现频率无法离线统计。
			st.noteUpstreamError(kind.String(), upstream.BusinessCode(string(respBody)))
			// 排队等待时长回落 body：上游可能在 data.retry_after 里给（HTTP 头未必有），
			// 队列语义下这个值是给客户端“何时重试”的唯一依据，不能丢。
			if kind == upstream.ErrModelQueued && uerr.RetryAfter <= 0 {
				if d := upstream.QueueRetryAfter(string(respBody)); d > 0 {
					uerr.RetryAfter = d
				}
			}
			// 内容拦截（11128 等）：这是**内容终态**，不轮转、不罚号、不自动改写提示词。
			//   - 不轮转：同一 body 换任何账号都撞同一审核；
			//   - 不罚号：账号余额/session 健康，问题在请求内容（见 applyErrorPolicy 分支）；
			//   - 不自动重试：自救路径是配置侧把 prompt.mode 改为 replace/after
			//     （system/developer 来源的指纹），而不是网关偷偷替换用户的系统提示词。
			if kind == upstream.ErrContentBlocked {
				// 内容命中网关内容防火墙：立即回客户端，**不轮转**——换任何账号都会撞同一
				// 审核，轮转纯属浪费时间。不罚账号（ErrContentBlocked 分支无冷却/熔断/NoteError）。
				// error-passthrough：message 装上游 body 原文（code/msg/requestId 原样），
				// 不再改写成网关固定文案——客户端必须看到真实错误才能排查。
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					// 空 body 兜底：无上游原文可透传，保留可读分类文案（不编造原文）。
					msg = "content blocked by upstream content firewall"
				}
				out.ErrorHint(w, http.StatusBadRequest, "content_blocked", msg,
					h.hintOf(upstream.ErrContentBlocked, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// 11115「prompt is too long」：立即透传上游原文回客户端，**不罚号不轮转**
			// ——上下文超限是请求的问题（同一 body 换任何号都超限，白扔健康号配额；
			// 与 WAF IP fail-fast 同哲学：确定与账号无关的错误直接终止轮转）。
			// applyErrorPolicy ErrPromptTooLong 分支零动作，fail 只释放租约。
			// message 装上游 body 原文（含真实 token 数与上限值——上游原文是最有价值
			// 的错误信息，客户端必须看到，禁止固定词覆盖）。
			if kind == upstream.ErrPromptTooLong {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				out.ErrorHint(w, http.StatusBadRequest, "prompt_too_long", promptTooLongMessage(string(respBody)),
					h.hintOf(upstream.ErrPromptTooLong, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// 图片格式/数据无效：立即透传上游原文回客户端，不罚号不轮转。
			// 同一 body 换账号仍是同样的解析结果，轮转只会放大无效请求。
			if kind == upstream.ErrImageInvalid {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "image request was rejected by upstream"
				}
				out.ErrorHint(w, http.StatusBadRequest, "image_invalid", msg,
					h.hintOf(upstream.ErrImageInvalid, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// 请求体解析失败（11101）：与 11115 / 图片无效同一哲学——同一 body 换任何
			// 账号都是同样的解析结果，轮转只会放大无效请求（每号一次上游调用 +
			// rotateBackoff 占用在途名额）。更要紧的是：继续轮转后末端会落到
			// 「其余保持 503」，把确定失败的请求伪装成"账号不可用、稍后再试"，
			// 客户端于是对必然失败的请求无限重试。立即透传上游原文回 400。
			if kind == upstream.ErrBadParams || kind == upstream.ErrClient {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "chat request body was rejected by upstream"
				}
				out.ErrorHint(w, http.StatusBadRequest, "bad_params", msg,
					h.hintOf(upstream.ErrBadParams, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// lastErr 携带完整 body（uerr.Msg 在 upstream 侧截断 200 字符，透传语义
			// 要求原文全量）+ Kind/RetryAfter（末端映射与冷却时长共用）。
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody), RetryAfter: uerr.RetryAfter}
			h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
			fail(acct.UID)
			// Only an explicit pre-generation model rejection is safe to replay.
			// 其余分类错误（含 WAF 403）都是**终态**：同一 body 换号重试只会放大请求量，
			// 直接透传上游原文回客户端。
			if kind != upstream.ErrModelBlocked {
				st.status = upstream.ErrorStatus(kind)
				st.outcome = reqlog.OutcomeHTTPError
				errBody := upstream.ErrorResponse(kind, string(respBody))
				if kind == upstream.ErrModelQueued {
					upstream.SetQueueRetryAfter(w.Header(), uerr.RetryAfter)
					errBody = upstream.QueueErrorResponse(string(respBody), uerr.RetryAfter)
				} else if uerr.RetryAfter > 0 {
					w.Header().Set("Retry-After", fmt.Sprint(int(uerr.RetryAfter.Seconds())+1))
				}
				out.JSON(w, st.status, errBody)
				return
			}
			// 仅 ErrModelBlocked（11102「该后端无此模型」）继续换号：它可能是账号维度的，
			// 换号有意义。
			if !rotateBackoff(i, r.Context()) {
				break // ctx 取消：终止轮转（分类错误换号退避）
			}
			continue
		}
		// 成功判定与粘性绑定一律**延后到这一跳真正成功之后**（见下方流式/非流式分支）：
		// 上游「200 已开流 + 一帧 error」是真实形态（6004 限流、内容拦截、审核），
		// 此前在读第一帧之前就 NoteSuccess + 清 11102 负缓存 + 绑粘性 → 被限流的号
		// 记成健康、粘性把会话钉死在它身上，后续每一轮都打同一个限流号。
		if peek.Stream {
			// 流式：透传结束后立即关闭上游 body，避免 defer 在轮转场景下堆积 fd。
			st.status = http.StatusOK
			stats := &chatStatsReader{start: st.start}
			timeouts := h.cfg.Upstream.StreamTimeoutsNow()
			// gateway_hint（SSE）：成功状态 200 已开流，中途 error 帧透传时附加
			// hint 字段（hintFn 惰性求值——正常流零开销，只有真撞到 error 帧才
			// 组装请求上下文做判定）。
			// errFrame：上游 error 帧原文（观察者旁路采集），用于流尾的账号处置。
			var errFrame string
			// 标记还原：上游在没有 tools 的请求里会把工具调用吐成原生标记文本，
			// 这里在透传前还原成 delta.tool_calls（见 internal/upstream/dsml.go）。
			// 每次 attempt 新建还原器（状态机按流持有，轮转重试不跨流）；弱判定
			// 常开——tools 声明缺失恰是泄漏的常态，名单为空也由块结构把关。
			repair := upstream.NewMarkupRepair(markupAllow, true)
			sErr := out.Stream(w, rc, upstream.FrameHintFunc(func() upstream.HintContext {
				return h.hintContext(bareModel, reqHasImage)
			}), upstream.WithErrorFrameObserver(func(payload string) { errFrame = payload }), upstream.WithExpectedChoices(peek.N), upstream.WithFrameObserver(stats.Observe), upstream.WithResponseTimeouts(timeouts.FirstModelEvent, timeouts.FirstGeneration, timeouts.Tail), upstream.WithMarkupRepair(repair))
			switch {
			case upstream.IsEmptyStreamError(sErr):
				// 上游 200 但空流（0 有效帧）：StreamHint 已写 error 帧 + [DONE]
				// 兜底（HTTP 头已发出只能 200），但这是上游缺陷不是成功——日志/
				// 状态收敛到 502 观测，与非流式 Aggregate 空流→502 upstream_parse
				// 同语义（此前 `_ =` 吞错把失败流记成 200，运维看到假成功）。
				// 只认 IsEmptyStreamError：客户端断连的写失败不误标（人已走，
				// 502 观测没有意义）。
				st.status = http.StatusBadGateway
				st.outcome = reqlog.OutcomeStreamError
				log.Printf("WARN: [server] stream acct=%s model=%s: empty upstream stream (200+0 frames)", logfmt.Label(acct.UID, acct.NicknameValue()), bareModel)
			case errFrame != "":
				// 上游以 error 帧报错（6004 限流 / 内容拦截 / 审核）：按帧内容分类并
				// 处置账号——**不记成功、不清 11102 负缓存、不绑粘性**。此前这些动作
				// 在流开始前就做了，于是一个正在限流的号被当成健康号，粘性还会把
				// 整个会话钉在它身上，后续每轮都失败。
				kind := upstream.FrameKind(errFrame)
				h.applyErrorPolicy(acct.UID, kind, errFrame, bareModel, nil)
				st.noteUpstreamError(kind.String(), upstream.BusinessCode(errFrame))
				st.status = upstream.ErrorStatus(kind)
				st.outcome = reqlog.OutcomeStreamError
				log.Printf("WARN: [server] stream acct=%s model=%s: upstream error frame kind=%s payload=%s",
					logfmt.Label(acct.UID, acct.NicknameValue()), bareModel, kind, logfmt.Truncate(errFrame, 200))
			case sErr != nil:
				var we *upstream.DownstreamWriteError
				if errors.As(sErr, &we) || r.Context().Err() != nil {
					st.outcome = reqlog.OutcomeInterrupted
				} else {
					st.status = http.StatusBadGateway
					st.outcome = reqlog.OutcomeStreamError
				}
			default:
				// 真成功：这一跳读完且上游没有报错，才记成功并让粘性跟上。
				st.outcome = reqlog.OutcomeSuccess
				st.noteUpstreamError("", "")
				h.cfg.Pool.NoteSuccess(acct.UID)
				// 11102 负缓存清命：该账号该模型实测成功，立即解除避让（不必等 TTL 到期）。
				// BlockModelClear 按 "11102" reason 前缀识别，只清 11102 条目、不碰 6004 独立冷却。
				h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
				// 粘性跟随最终成功号：本轮成功的账号成为该会话的粘性绑定（覆盖旧绑定）。
				// 若 sticky 号失败、轮换到别的号成功，这里把会话重绑到新号，多轮对话下一跳不再随机抽。
				// convKey 含 prompt_cache_key/派生键回退（75c0a78 断链恢复）：非 conversation
				// 维度键的会话同样要跟随成功号。
				if convKey != "" && h.cfg.Session != nil {
					actualSessKey := acct.Realm() + ":" + bareModel + ":" + convKey
					if principalID != "" {
						actualSessKey = principalID + ":" + actualSessKey
					}
					if sessKey != "" && sessKey != actualSessKey {
						h.cfg.Session.Unbind(sessKey)
						sessKey = actualSessKey
					}
					h.cfg.Session.Bind(sessKey, acct.UID)
					// 指纹记录：派生键会话成功后快照消息窗口，供断链后的窗口匹配找回。
					if isDerivedKey {
						h.cfg.Session.RecordFingerprint(sessKey, principalID, bareModel, peek.Object)
					}
				}
			}
			// 标记还原观测（§15 显式可见）：命中写入归档改写列（面板可见）+ 日志一行
			// 便于统计发生率；识别到却没还原（判定不通过）单独 WARN，排障时能区分
			//「没识别到」与「识别到但拒绝」。
			if n := repair.Converted(); n > 0 {
				st.noteRewrite(fmt.Sprintf("tool_calls.markup:leaked→%dcall(s)", n))
				log.Printf("INFO: [server] stream acct=%s model=%s: repaired %d native tool call(s) from assistant text (tool names known=%d)",
					logfmt.Label(acct.UID, acct.NicknameValue()), bareModel, n, len(markupAllow))
			} else if m := repair.Seen(); m > 0 {
				log.Printf("WARN: [server] stream acct=%s model=%s: saw %d native tool call block(s) but repaired none (tool names known=%d)",
					logfmt.Label(acct.UID, acct.NicknameValue()), bareModel, m, len(markupAllow))
			}
			credit, hasCredit := stats.Credit()
			if hit, miss, ok := stats.CacheTokens(); ok {
				st.cacheHit, st.cacheMiss, st.hasCache = hit, miss, true
			}
			recordAttempt(acct.UID, stats.Usage(), credit, hasCredit, attemptStarted, stats.TTFB())
			// WARN 信号放 recordAttempt 之后：st.promptTokens 此时才是本次的观测值。
			if st.hasCache {
				cacheMissWarn.noteCacheTokens(bareModel, st.promptTokens, st.cacheHit, st.cacheMiss)
			}
			st.ttfb = stats.TTFB()
			// usage 缺失时保留 chatStat.toks 的 -1 哨兵（观测缺失 → 显示 "-"），
			// 不写入零值——否则「没观测到 usage」被伪造成「测得 0 token」，
			// 与非流式走 completionTokens 返回 -1 的口径不一致。
			if toks, hasUsage := stats.Tokens(); hasUsage {
				st.toks = toks
			}
			// 成本账本：末帧 usage 带 credit 与 token 总数时记录实测单价，
			// 供下次选号把免费/便宜的号排在前面。
			if hasCredit {
				st.credit = credit
				st.hasCredit = true
				if total, tok := stats.TotalTokens(); tok && total > 0 {
					h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, total)
				}
			}
			rc.Close()
			return
		}
		observed := &chatStatsReader{start: st.start}
		timeouts := h.cfg.Upstream.StreamTimeoutsNow()
		// 标记还原（非流式）：同流式，每次 attempt 新建还原器，弱判定常开。
		repair := upstream.NewMarkupRepair(markupAllow, true)
		completion, err := out.Aggregate(rc, upstream.WithExpectedChoices(peek.N), upstream.WithFrameObserver(observed.Observe), upstream.WithResponseTimeouts(timeouts.FirstModelEvent, timeouts.FirstGeneration, timeouts.Tail), upstream.WithMarkupRepair(repair))
		rc.Close()
		var formatted map[string]any
		if err == nil {
			formatted, err = completion.Format(request)
		}
		if err != nil {
			credit, hasCredit := observed.Credit()
			recordAttempt(acct.UID, observed.Usage(), credit, hasCredit, attemptStarted, 0)
			// 上游流解析失败：客户端还没看到任何输出，回 502 并告知原因。
			var fe *upstream.FrameError
			if errors.As(err, &fe) {
				h.applyErrorPolicy(acct.UID, fe.Kind, fe.Payload, bareModel, nil)
				st.noteUpstreamError(fe.Kind.String(), upstream.BusinessCode(fe.Payload))
				errBody := upstream.ErrorResponse(fe.Kind, fe.Payload)
				if fe.Kind == upstream.ErrModelQueued {
					upstream.SetQueueRetryAfter(w.Header(), fe.RetryAfter)
					errBody = upstream.QueueErrorResponse(fe.Payload, fe.RetryAfter)
				}
				out.JSON(w, fe.Status, errBody)
				st.status = fe.Status
			} else {
				out.Error(w, http.StatusBadGateway, "upstream_parse", err.Error())
				st.status = http.StatusBadGateway
			}
			st.outcome = reqlog.OutcomeHTTPError
			return
		}
		resp := completion.Raw
		// 标记还原观测（非流式，与流式同口径）：命中写归档改写列 + 日志一行。
		if n := repair.Converted(); n > 0 {
			st.noteRewrite(fmt.Sprintf("tool_calls.markup:leaked→%dcall(s)", n))
			log.Printf("INFO: [server] non-stream acct=%s model=%s: repaired %d native tool call(s) from assistant text (tool names known=%d)",
				logfmt.Label(acct.UID, acct.NicknameValue()), bareModel, n, len(markupAllow))
		} else if m := repair.Seen(); m > 0 {
			log.Printf("WARN: [server] non-stream acct=%s model=%s: saw %d native tool call block(s) but repaired none (tool names known=%d)",
				logfmt.Label(acct.UID, acct.NicknameValue()), bareModel, m, len(markupAllow))
		}
		credit, total, hasCredit := usageCreditTotal(resp)
		if usage, ok := resp["usage"].(map[string]any); ok {
			if hit, okH := upstream.UsageCacheHitTokens(usage); okH {
				miss, _ := upstream.UsageCacheMissTokens(usage)
				st.cacheHit, st.cacheMiss, st.hasCache = int64(hit), int64(miss), true
			}
		}
		recordAttempt(acct.UID, usageDeltaFromResponse(resp), credit, hasCredit, attemptStarted, 0)
		if st.hasCache {
			cacheMissWarn.noteCacheTokens(bareModel, st.promptTokens, st.cacheHit, st.cacheMiss)
		}
		writeJSON(w, http.StatusOK, formatted)
		st.status = http.StatusOK
		st.outcome = reqlog.OutcomeSuccess
		st.noteUpstreamError("", "")
		// 下游交付失败（慢/已断连客户端）：上游确实生成成功，但客户端没拿到正文。
		// 不区分的话状态码 200 被当成成功，运维在日志/请求记录里根本看不出这次请求
		// 结果丢在了网关出口。此处只改观测口径：**不得**重放上游（可能重复扣费）。
		// 写失败发生在写 deadline 之后（writeDeadlineWriter 记录首次错误）。
		if dw, ok := w.(*writeDeadlineWriter); ok && dw.err != nil {
			st.status = http.StatusBadGateway
			st.outcome = reqlog.OutcomeInterrupted
			log.Printf("WARN: [server] downstream write failed acct=%s model=%s: %v (upstream succeeded, response not delivered)",
				logfmt.Label(acct.UID, acct.NicknameValue()), bareModel, dw.err)
		}
		st.toks = completionTokens(resp)
		// 非流式同理：聚合成功（无 error 帧、非空流）才算这一跳成功，事后才记成功/绑粘性。
		h.cfg.Pool.NoteSuccess(acct.UID)
		h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
		// 粘性跟随最终成功号（非流式同理）：convKey 含回退链（见流式分支注释）。
		if convKey != "" && h.cfg.Session != nil {
			actualSessKey := acct.Realm() + ":" + bareModel + ":" + convKey
			if principalID != "" {
				actualSessKey = principalID + ":" + actualSessKey
			}
			if sessKey != "" && sessKey != actualSessKey {
				h.cfg.Session.Unbind(sessKey)
				sessKey = actualSessKey
			}
			h.cfg.Session.Bind(sessKey, acct.UID)
			// 指纹记录（非流式同理）：仅派生键会话。
			if isDerivedKey {
				h.cfg.Session.RecordFingerprint(sessKey, principalID, bareModel, peek.Object)
			}
		}
		// 成本账本（非流式）：从聚合响应的 usage 取 credit 与 token 总数。
		if hasCredit {
			st.credit = credit
			st.hasCredit = true
			h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, total)
		}
		return
	}
	// 末端错误透传（error-passthrough）：上游返回的错误原样透传，不再规范化成固定文案。
	// 上游返回（*upstream.Error）→ error.message 装**上游 body 原文**（code/msg/
	// requestId 原样保留）。HTTP 状态码按 OpenAI 兼容口径映射类别：ErrSoftRate → 429
	// （限流语义、客户端应等待重试），其余保持 503。本地调度类错误（无可用账号/
	// 传输层抖动/非上游返回的 lastErr）→ 保留自有文案 no_healthy_account（本地错误
	// 没有上游原文可透传，不编造）。
	status := http.StatusServiceUnavailable
	code := "no_healthy_account"
	msg := "all accounts are temporarily unavailable, please retry later"
	// 上游超时：轮转已在传输层分支止损（见 isUpstreamTimeout），这里给一条**能区分**
	// 的文案，别混进"没有可用账号"——两者的排查方向完全不同。
	if errors.Is(lastErr, errUpstreamTimeout) {
		code = "upstream_timeout"
		msg = "upstream timed out: rotation stopped (another account would hit the same slow upstream), please retry later"
	}
	// gateway_hint（末端透传）：上游错误按 Kind + 原文 + 请求形态判定；本地调度类
	// 错误（无上游原文）固定 no_healthy_account hint。
	hint := upstream.NoHealthyAccountHint()
	// upstreamMsgPassed 记录 error.message 是否已被上游原文占据：模型级阻塞分支
	// 据此决定要不要覆盖 msg（上游原文优先，含 requestId）。
	upstreamMsgPassed := false
	var ue *upstream.Error
	if errors.As(lastErr, &ue) {
		hint = h.hintOf(ue.Kind, ue.Msg, bareModel, reqHasImage, ue)
		switch ue.Kind {
		case upstream.ErrSoftRate:
			status = http.StatusTooManyRequests
			code = "rate_limit_exceeded"
			msg = "rate limited: all accounts are cooling down, please wait a moment and try again"
		case upstream.ErrWafBlock:
			// WAF 403 已在轮转循环内按终态直接透传（不轮转），此处仅为分类映射冗余分支；
			// 保留文案以便未来若有路径汇入末端时仍有可读提示（不再宣扬 IP 级判定）。
			code = "waf_blocked"
			msg = "upstream firewall blocked the request; rotation stopped"
		case upstream.ErrModelBlocked:
			// 11102「该后端无此模型」：上游原文（下方统一透传）已经写清了原因，但
			// code 此前停在默认的 no_healthy_account —— 那是"服务端过载"的语义，
			// 客户端据此会不断重试（#81 里就是 503 → 客户端自动重试 5 次），而真正
			// 该做的是换个模型。用 400 + 明确的 code 把不可重试的性质讲清楚。
			code = "model_unavailable"
			status = http.StatusBadRequest
		}
		if s := strings.TrimSpace(ue.Msg); s != "" {
			// 上游原文优先：透传 code/msg/requestId，不拼接本地前缀。
			msg = s
			upstreamMsgPassed = true
		}
	}
	// 模型级阻塞覆盖上面的通用文案（放在最后 = 优先级最高）。
	//
	// 为什么能覆盖 code 而不丢信息：modelBlock.Reason 就是 BlockModelBackoff 存下的
	// 上游原因，已在 hint 里原样带出，所以覆盖 message 不会丢掉上游信息，反而补上
	// 了上游不会告诉客户端的两件事——「有几个号被挡」和「最早什么时候解封」。
	//
	// 用 400 而非 503：这是"你选的这个模型当前不可用"，不是"服务端暂时过载"。
	// 换号重试必然同样失败，客户端不该按可重试错误处理。
	if modelBlock.Blocked {
		code = "model_unavailable"
		// 有上游原文时保留它（含 requestId，用户要拿去向上游反馈），只把"有几个号
		// 被挡、最早何时解封"这类本地调度信息放进 gateway_hint。
		if !upstreamMsgPassed {
			msg = "model is unavailable on every account (per-model cooldown), try another model"
		}
		hint = fmt.Sprintf("model_blocked: %d account(s) cooling down this model", modelBlock.Count)
		if !modelBlock.Until.IsZero() {
			hint += "; earliest unblock at " + modelBlock.Until.Format(time.RFC3339)
		}
		if s := strings.TrimSpace(modelBlock.Reason); s != "" {
			hint += "; upstream: " + s
		}
		status = http.StatusBadRequest
	}
	out.ErrorHint(w, status, code, msg, hint)
	st.status = status
	st.outcome = reqlog.OutcomeHTTPError
}

// tokensPerSecond 计算吐字速率（token/s），返回 (速率, 是否有意义)。
//
// 分母用「生成耗时」= 端到端耗时 - 首 token 等待（TTFB），不是端到端耗时。
//
// 为什么要减：不减的话，首 token 等待越长、报告速率被压得越低。同一模型换个
// 上游或网络，TTFB 从 0.3s 涨到 2s，速率能凭空掉一半——读起来像"模型变慢了"，
// 其实只是排队久了。issue #34 报的正是这个，维护者也确认「速率计算时并没有减去
// 首token到达时间」。
//
// ttfb<=0 表示没有观测：非流式回复天然没有「首个 data 帧」（日志里那一列记的是
// "-"）。此时不猜、不扣——凭空假定一个 TTFB 会把分母推向零、把速率抬成虚高，
// 比不扣更糟。只有真测到才扣。
//
// 同理，ttfb 不小于总耗时时（时钟粒度、或 TTFB 落在计时终点之后）退回端到端耗时，
// 避免零/负分母。token 数为负哨兵值（-1 = 观测缺失）时返回 false。
//
// 扣除后不足 minGenWindow 也退回端到端（issue #127）：ttfb 量的是「首个 SSE 帧
// 到达」，当上游把整个响应攒到最后一次性下发（假流式/中间层攒批刷新——首帧与
// 末帧几乎同时到）时，total−ttfb 只剩几毫秒，拿它当分母会把几百 token 除成
// 上万 tok/s 的幻数。这种形态下「生成时长」根本不可测，诚实的分母只有端到端。
//
// 用量账本（handler）与控制台流水行（logging.go）都走这一个函数：两处各算一遍时
// 口径漂移过一次（流水行漏扣 TTFB、与面板数字对不上），共用是防再次分叉的唯一办法。
func tokensPerSecond(completionTokens int64, total, ttfb time.Duration) (float64, bool) {
	if completionTokens < 0 || total <= 0 {
		return 0, false
	}
	gen := total
	if ttfb > 0 {
		if g := total - ttfb; g >= minGenWindow {
			gen = g
		}
	}
	return float64(completionTokens) / gen.Seconds(), true
}

// minGenWindow 可信生成窗口的下限：扣除 TTFB 后剩余窗口不足该值即视为「生成
// 时长不可测」，退回端到端耗时（见 tokensPerSecond 注释，issue #127）。
// 真流式下首帧到末帧通常铺满剩余窗口，200ms 远低于正常生成时长，不影响真实
// 快速输出（如缓存命中后的爆发）被如实报告。
const minGenWindow = 200 * time.Millisecond

// promptTooLongMessage 11115 透传 message：上游 body 原文（含真实 token 数/
// 上限值/requestId，客户端自行排查）；空 body 兜底为可读分类短文案（不编造原文）。
func promptTooLongMessage(body string) string {
	if strings.TrimSpace(body) == "" {
		return "prompt is too long"
	}
	return body
}

// usageCreditTotal 从聚合响应取 usage.credit 与 total_tokens（成本台账非流式入口）。
// 任一字段缺失/非法 → ok=false（不记录）。
func usageCreditTotal(resp map[string]any) (credit float64, total int, ok bool) {
	usage, _ := resp["usage"].(map[string]any)
	if usage == nil {
		return 0, 0, false
	}
	c, hasCredit := usage["credit"].(float64)
	if n, yes := usage["credit"].(json.Number); yes {
		var err error
		c, err = n.Float64()
		hasCredit = err == nil
	}
	t, hasTotal := usage["total_tokens"].(float64)
	if n, yes := usage["total_tokens"].(json.Number); yes {
		v, err := n.Int64()
		t, hasTotal = float64(v), err == nil
	}
	if n, yes := usage["total_tokens"].(int64); yes {
		t, hasTotal = float64(n), true
	}
	if !hasCredit || !hasTotal || t <= 0 {
		return 0, 0, false
	}
	return c, int(t), true
}

// rotateBackoff 轮转间指数退避 + 抖动（WAF 403 修复 P0-2）：第 i 次轮转失败
// （continue 换号前）等待 backoffAfter(i)（500ms·2^i 封顶 8s，±25% 抖动），
// ctx 取消（客户端断连/优雅停机）返回 false——调用方立即终止轮转（客户端已走，
// 换号重试无意义）。退避是「换号前歇一下」让上游频控窗口滑过；正常单号请求
// （首次成功）不经过本函数，零开销。
func rotateBackoff(i int, ctx context.Context) bool {
	d := backoffAfter(i)
	if d <= 0 {
		return ctx.Err() == nil
	}
	if !sleepCtx(ctx, d) {
		log.Printf("WARN: [server] rotate backoff aborted: ctx cancelled")
		return false
	}
	return true
}

// errUpstreamTimeout 上游超时的哨兵：末端出口据此给出与「没号可用」可区分的文案。
var errUpstreamTimeout = errors.New("upstream timeout")

// isUpstreamTimeout 判断这一跳的失败是否属于「上游超时 / 停滞」。超时不是账号的
// 问题，换号注定白换（同一份请求撞同一个慢上游），必须止损：不轮转、不罚号。
// 三态判定见传输层错误分支的注释。
func isUpstreamTimeout(err error, clientGone bool) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	return !clientGone && errors.Is(err, context.Canceled)
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// kind 是唯一权威分类（来自 upstream.Classify / ChatStreamContext 的 *Error 信封），
// 此处不再按原始 status 二次判断。仅在 chatCompletions 轮转循环内调用：内容拦截
// 会立即 400 返回，其余种类 continue 换号（continue 前由 rotateBackoff 退避）。
//
// 十条路径，各司其职：
//   - ErrHardCredit → CooldownUntilTomorrow4AM：即时硬冷却到次日 04:00（等签到恢复）。
//   - ErrSoftRate → 优先对齐上游重置墙钟（带「将在 … 重置」时 6004 走模型级豁免、
//     非 6004 走账号级，均不指数堆加）；无重置时间才走有界退避。冷却时长优先采信
//     Retry-After 头（uerr.RetryAfter，body 文案墙钟之外的头形态来源）。
//   - ErrWafBlock → 账号级软冷却：**不 Disable**——WAF 403 是 IP/指纹维频控信号，
//     罚过即走、到期自愈。时长优先 Retry-After 头；缺失按 wafCooldownBase(60s)
//     起 · softStreak 指数、封顶 soft_rate_max 的既有 CooldownSoftRate 有界退避。
//     基数经 jitterDur 抖动（防多账号同相位冷却到期再聚团）。
//   - ErrNotFound → Cooldown(CoolSoft, notFoundCooldown 固定 60s)：短冷却防雪崩。
//   - ErrSessionDead → Disable：session 死亡，永久禁用（需人工重登）。
//   - ErrContentBlocked → 不罚账号；passthrough 首遇触发降级重试，最终仍拦则回 400。
//   - ErrBadParams → 不罚账号，且与 ErrPromptTooLong/ErrImageInvalid 同待遇：
//     调用方在轮转循环内即刻 400 透传原文终止（换号必然同样失败）。
//   - ErrPromptTooLong → 11115：请求的问题不是账号的问题。零动作（不冷却/不熔断/
//     不 NoteError、不喂连败），chatCompletions 已直接透传原文返回不轮转。
//   - ErrImageInvalid → 图片格式/数据无效：请求的问题不是账号的问题（同一 body
//     换任何号都会得到相同的解析错误）。零动作（不冷却/不熔断/不 NoteError、
//     不喂连败），chatCompletions 已直接透传原文返回不轮转。
//   - ErrModelBlocked → BlockModelBackoff：(账号, 模型) 11102 负缓存避让。
//   - ErrServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。
//   - 其他（default：ErrClient/ErrNone）→ 只换号不罚（防雪崩），不喂熔断；ErrClient
//     额外喂连败计数（NoteFailures，issue #114）：未知 4xx 连败 N 次临时出池。
//
// body 仅在 ErrSoftRate/ErrAccountFault 分支用于解析重置时间/分野；model 为请求
// 携带的模型名。uerr 是 ChatStreamContext 返回的分类信封（可携带 RetryAfter）；
// 零值/防御路径下为 nil，冷却时长回落既有计算。
func (h *Handler) applyErrorPolicy(uid string, kind upstream.ErrKind, body, model string, uerr *upstream.Error) {
	switch kind {
	case upstream.ErrHardCredit:
		// 402 + 余额关键词即积分耗尽：同步冷却到次日 04:00（签到任务 09/21 点恢复），
		// 不需要异步核查（冗余）。立即换号。
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "余额不足")
	case upstream.ErrSoftRate:
		// 统一对齐上游重置时间：只要 body 带「将在 … 重置」，无论业务 code 是
		// 6004 还是 11140 rate-limiting 等形态，都精确冷却到该墙钟、绝不指数堆加。
		//   - 模型级（6004）→ CooldownSoftForModel：写 modelCooldowns[model]，切模型豁免。
		//   - 账号级（非 6004）→ CooldownSoftRate：写账号级 until，不产生模型豁免。
		modelRateLimited := upstream.IsModelRateLimit(body)
		if resetAt, ok := upstream.ParseRateReset(body); ok {
			if modelRateLimited {
				h.cfg.Pool.CooldownSoftForModel(uid, h.softCooldown(), resetAt, model, "6004 model rate limit")
				return
			}
			h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), resetAt, "429 rate limit")
			return
		}
		// body 无重置文案但带 Retry-After 头 → 冷却到该时刻（不做指数堆加）。
		// 头优先于「有界退避」，但低于 body 重置文案（文案是上游更权威的口径）。
		if uerr != nil && uerr.RetryAfter > 0 {
			h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), time.Now().Add(uerr.RetryAfter), "429 rate limit (retry-after)")
			if modelRateLimited {
				h.cfg.Pool.RecordModelRateLimitAudit(uid, model, "6004 model rate limit (reset unknown)")
			}
			return
		}
		// 无重置时间 → 账号级有界退避（soft_rate 基数起、softStreak 翻倍、封顶
		// soft_rate_max；已在冷却中的兜底探测不翻倍）。基数取 h.softCooldown()
		// （热改优先），管理面板改 soft_rate 后立即生效。
		h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), time.Time{}, "429 rate limit")
		if modelRateLimited {
			h.cfg.Pool.RecordModelRateLimitAudit(uid, model, "6004 model rate limit (reset unknown)")
		}
	case upstream.ErrWafBlock:
		// WAF 403（无业务信封拦截形态）。软冷却复用 CooldownSoftRate 家族：基数
		// wafCooldownBase（60s，抖动后落 [45s,75s]）、softStreak 指数升级、封顶
		// soft_rate_max、冷却中兜底探测不翻倍——全部继承既有语义。
		// Retry-After 头优先（WAF 拦截页可能带该头）。不 Disable。
		if uerr != nil && uerr.RetryAfter > 0 {
			h.cfg.Pool.CooldownSoftRate(uid, jitterDur(wafCooldownBase), time.Now().Add(uerr.RetryAfter), "waf 403 block (retry-after)")
			return
		}
		h.cfg.Pool.CooldownSoftRate(uid, jitterDur(wafCooldownBase), time.Time{}, "waf 403 block")
	case upstream.ErrSessionDead:
		h.cfg.Pool.Disable(uid, "12153 session dead")
	case upstream.ErrNotFound:
		// Endpoint failure is not an account failure.

	case upstream.ErrAccountFault:
		// 账号级授权/配额故障按 msg 分野（口径与 Classify 的 accountFaultMarkers 一致）：
		//   - "request illegal"（code 11140）→ 账号级**授权封禁**：硬禁用（Disable）。
		//   - 14017（trial not activated）→ register 未完成，补完 register 后可能自愈，
		//     **保持软冷却**（禁用会让用户补完 register 后仍无法用）。
		// 大小写不敏感（与 Classify 的 marker 匹配同口径）。
		if strings.Contains(strings.ToLower(body), "request illegal") {
			h.cfg.Pool.Disable(uid, "account banned by upstream (11140 request illegal), re-login required")
			return
		}
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.softCooldown(), "account fault (14017)")
	case upstream.ErrServer:
		// Route/server errors must not poison account health.

	case upstream.ErrContentBlocked:
		// 内容策略拦截（误报）：内容问题非账号问题，不罚账号（无冷却/熔断/NoteError）。
		// passthrough 模式由 chatCompletions 内降级重试处理；custom 模式本不会到此分支。
	case upstream.ErrModelQueued:
		// 模型容量队列（6020/6021/6022 或 HTTP 202）：上游在排队，与账号健康完全
		// 无关——零动作（不冷却/不熔断/不 NoteError，同 ErrContentBlocked 待遇）。
		// 也**不轮转**（同 kind 在末端直接终止轮转）：6020/6021 是模型全局容量，
		// 换号不缩队；6022 虽然账号维度，但换号会白耗一个健康号额头并丢掉会话
		// 缓存，交给客户端按 Retry-After 重试更诚实（客户端自己决定要不要换模型）。
	case upstream.ErrPromptTooLong:
		// 11115「prompt is too long」：请求的问题不是账号的问题（同一 body 换任何
		// 号都超限）。零动作（不冷却/不熔断/不 NoteError，同 ErrContentBlocked 待遇），
		// chatCompletions 已直接透传原文返回不轮转——该分支只为文档完备。
	case upstream.ErrImageInvalid:
		// 图片格式/数据无效：请求的问题不是账号的问题（同一 body 换任何号都会
		// 得到相同解析错误）。零动作，chatCompletions 已 fail-fast 透传。
	case upstream.ErrBadParams, upstream.ErrClient:
		// 请求体解析失败（400 + Unmarshal chat params failed / 11101）：发给上游的 body
		// 有问题（网关侧不再截断，均为客户端畸形 JSON）。换了账号照样 400，
		// 不罚账号（无冷却/熔断/NoteError，同 ErrContentBlocked 待遇）；chatCompletions
		// 已 fail-fast 400 透传原文、终止轮转——「换号可能有不同模型权限」属 11102
		// （ErrModelBlocked）的分类域，与本类无关。
	case upstream.ErrModelBlocked:
		// 11102「该后端无此模型」：(账号, 模型) 负缓存避让。复用 modelCooldowns 机制
		// （与 6004 同域），选号侧 healthyForModel 对该账号自动避开该模型。
		// 立即换号（本轮 continue），该账号该模型冷却，下次选号避开。
		h.cfg.Pool.BlockModelBackoff(uid, model, upstream.ModelBlockReason)
	default:
		// Unknown failures do not establish an account fault.

	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}

// wafCooldownBase WAF 403 软冷却基数（建议 60s 起；抖动 ±25% 后落 [45s,75s]，
// 实际进入 CooldownSoftRate 后再按 softStreak 指数、封顶 soft_rate_max）。
// 与 SoftCooldown 分流的原因：WAF 403 是 IP/指纹维频控，信号比 429「账号级限流」轻
// （账号本身健康），但比 404 重（带粘性会连环）；60s 级的快速避让已足够让频控窗口
// 滑过。抖动复用 backoff.go jitterDur（单一来源）。
const wafCooldownBase = 60 * time.Second

// writeOpenAIErrorHint 同 writeOpenAIError，另在 error 对象上附加
// error.gateway_hint（hint 为空串时不带字段——未覆盖形态不编造）。
// message 仍是上游原文透传（hint 只做并列补充，绝不替换/包装 message）。
func writeOpenAIErrorHint(w http.ResponseWriter, status int, code, msg, hint string) {
	if hint == "" {
		writeOpenAIError(w, status, code, msg)
		return
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message":      msg,
			"type":         "api_error",
			"code":         code,
			"gateway_hint": hint,
		},
	})
}

// hintContext 组装 chatCompletions 的 gateway_hint 判定上下文：请求裸模型名 +
// 是否带图 + 模型目录 supports_images 声明（目录未收录 → ModelInCatalog=false，
// 不做「不支持」判定，防查不到误判）。仅错误路径调用（成功请求零开销）。
//
// 目录查询只读目录快照（upstream.Client.Catalog），**不触发任何上游拉取**：错误路径
// 加一次 FetchModels 网络调用既拖慢错误响应、又污染上游调用语义（错误风暴时放大
// 请求量——与 WAF IP fail-fast 的「不放大请求量」哲学相悖）。快照为空/未收录 →
// ModelInCatalog=false，11133 退中性 hint（宁缺勿滥，不编造能力事实）。
func (h *Handler) hintContext(bareModel string, hasImage bool) upstream.HintContext {
	ctx := upstream.HintContext{Model: bareModel, HasImage: hasImage}
	if bareModel == "" || h.cfg.Upstream == nil {
		return ctx
	}
	for _, mi := range h.cfg.Upstream.Catalog("cn").Infos {
		if mi.ID == bareModel {
			ctx.ModelInCatalog = true
			ctx.ModelSupportsImages = mi.SupportsImages
			return ctx
		}
	}
	return ctx
}

// hintOf 末端错误透传的统一 hint 入口：kind + 上游原文 + 请求上下文 →
// gateway_hint 文案（upstream.GatewayHint 单一事实来源）。uerr 为 nil 时回落
// body 原文判定（防御路径）。transport 层错误（lastErr 非 *upstream.Error 且
// 上游没回 body）→ 无 hint（不编造）。
func (h *Handler) hintOf(kind upstream.ErrKind, body, bareModel string, hasImage bool, uerr *upstream.Error) string {
	msg := body
	if uerr != nil && uerr.Msg != "" {
		msg = uerr.Msg
	}
	return upstream.GatewayHint(kind, msg, h.hintContext(bareModel, hasImage))
}

func containsRealm(realms []string, realm string) bool {
	for _, r := range realms {
		if r == realm {
			return true
		}
	}
	return false
}
