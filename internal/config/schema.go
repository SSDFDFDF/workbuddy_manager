// schema.go 配置结构定义与默认值（原 cmd/server/config.go 前半）。
//
// 本包（internal/config）是配置域的唯一真相：类型、默认值、加载、校验、
// 字段目录（catalog.go）、差异计算（diff.go）都在这里；具体组件的热应用动作
// 由 cmd/server/apply.go 通过 Applier 接口注入，本包不依赖 pool/scheduler/
// server/reqlog/session 等运行期组件。
package config

import (
	"time"

	"workbuddy_manager/internal/prompt"
	"workbuddy_manager/internal/proxy"
	"workbuddy_manager/internal/scrub"
	"workbuddy_manager/internal/upstream"
)

// 入站准入的缺省值（server.* 可覆盖）。与 internal/server 的默认值保持一致；
// 这里单独定义一份，避免 cmd/server 为了两个数字反向依赖 server 包的内部常量。
const (
	defaultMaxInflightRequests = 64
	defaultMaxInflightBytesMB  = 256
)

// CurrentVersion 本程序写文件时使用的配置结构版本（config_version 字段的取值）。
//
// 它只是一个标记 + 一道单向守卫，不再是迁移链的终点：读取时不按版本号做任何
// 形态转换（旧版本号 = 照当前结构读，认识的键生效、其余忽略），只有比它更新的
// 版本会被拒绝。schema 发生不兼容变更时手动 +1 即可，无需配套迁移代码。
const CurrentVersion = 3

// Config 顶层配置。
type Config struct {
	// ConfigVersion 配置文件的结构版本：本程序写文件时固定写 CurrentVersion。
	// 读取时不做任何版本转换，只在「文件声明的版本比程序新」时拒绝启动
	//（旧程序读新配置会静默丢掉不认识的键，那种数据损失必须 fail fast）。
	ConfigVersion int    `json:"config_version"`
	Listen        string `json:"listen"`  // ":7863"
	APIKey        string `json:"api_key"` // 空 = 不鉴权
	// AuthDir 账号凭证目录（./auths）。保存后热生效：重扫目录 + 账号池对齐
	//（auth.LoadDir / pool.SyncToDir），面板登录与导入随之写入新目录。
	AuthDir   string `json:"auth_dir"`   // ./auths
	StateFile string `json:"state_file"` // ./data/state.json

	// FingerprintRewrite 出站请求体指纹改写（默认 false = 严格逐字透传）。
	//
	// 开启后按内置指纹表（internal/scrub，共 10 条：7 条逐字 + 3 条变长模式）
	// 改写 user/assistant/tool/推理/工具入参及 tools 声明里的已知串——
	// Claude Code / Claude Agent SDK / Codex 身份句、billing-header 键值、
	// 尾随 cc_ 键值、反馈整句、裸 11128。这些串出现在 system/developer 以外
	// 的来源时 prompt.mode 清不掉，只能逐字改写。
	//
	// 代价：**会改用户可见内容**（如对话里的 11128 变成 11-128），
	// 因此默认关闭；面板可热改，无需重启。
	FingerprintRewrite bool `json:"fingerprint_rewrite"`

	// FingerprintDisabled 禁用的内置指纹规则 ID 清单（空 = 全部内置规则启用）。
	//
	// 合法 ID 见 scrub.BuiltinRules()（面板「管理内置指纹表」可视化编辑）；
	// 未知 ID 在 normalize 阶段告警并剔除。停用 err-11128 会额外告警
	// （请求中出现裸 11128 将被上游整单拦截）。
	FingerprintDisabled []string `json:"fingerprint_disabled,omitempty"`

	// FingerprintOverrides 逐内置规则的替换文本覆盖（键 = rule_id，值 = 新替换文本）；
	// 空 = 全部用内置默认。未知 ID 告警并剔除；要停用某条规则请用 FingerprintDisabled。
	FingerprintOverrides map[string]string `json:"fingerprint_overrides,omitempty"`

	// FingerprintRules 自定义改写规则（在内置指纹之上**叠加**）。
	//
	// 用于把“自己遇到的指纹词/句”加进来，例如某个客户端的特殊标记、
	// 自己的项目名不能出现在请求里、某个内部术语要替换后再出站。
	// 每条规则的 Match/Replace/Mode/Action 见 internal/scrub.Rule，
	// 支持 literal（大小写敏感）/ fold（忽略 ASCII 大小写）两种匹配，
	// replace（换成给定文本）/ remove（整段删除）两种动作。
	//
	// 上限与合法性在 normalize 阶段校验（超量/非法 → 启动或保存报错）。
	// 改动**热生效**（与 FingerprintRewrite 同步），无需重启。
	FingerprintRules []scrub.Rule `json:"fingerprint_rules,omitempty"`

	Panel struct {
		// PackageDetailLimit 积分构成页单账号默认展示的最近到期包数；<=0 回落 5。
		PackageDetailLimit int `json:"package_detail_limit"`
	} `json:"panel"`

	Media struct {
		// ToolImages 工具结果图片处理策略（internal/media）：
		//   "" / "auto"    —— 按入口默认：原生 Chat 透传（不改既有转发行为），
		//                     Responses / Messages 抬升为工具批次后的 user 图片消息；
		//   "passthrough"  —— 全部入口原样转发 tool 消息里的图片 part；
		//   "hoist"        —— 全部入口抽出图片，改为工具批次之后的 user 图片消息；
		//   "reject"       —— 全部入口明确 400（不转发工具结果图片）。
		//
		// 抬升是官方 custom-model-tool-media-hoist 插件同构的显式兼容变换（改变图片的
		// 角色与上下文位置，不是无损编码）；透传则把未验证的 tool 多模态形态交给上游
		// 判定。改动**热生效**（进程级策略，无需重启）。
		ToolImages string `json:"tool_images"`

		// ImageTranscode 把上游不支持的图片格式（gif/bmp/tiff）转码为 PNG（无损；
		// 若同时开启压缩档，转码结果可能进一步被重编码为更小的 JPEG）。
		//
		// GIF 只保留首帧（动画信息丢失，官方客户端转 PNG 同样如此）；Go 无 webp
		// 编码器，webp 仅在开启压缩档时被重编码为 JPEG。默认 false。
		// 改动**热生效**。
		ImageTranscode bool `json:"image_transcode"`

		// ImageMaxDimension 图片压缩档位：0 = 关闭（默认），1080 / 2000 为内置两档
		// （对齐官方 CODEBUDDY_CODE_IMAGE_COMPRESSION_MAX_DIMENSION：1080 档 base64
		// ≤512000 + JPEG 质量 [85,70,50,30]；2000 档 base64 ≤5 MiB + 质量
		// [80,60,40,20]）。开启后超过档位边长或体积的图片会被等比缩放 + JPEG 重编码
		// （有损）——这是显式声明的兼容变换，不是无损编码。改动**热生效**。
		ImageMaxDimension int `json:"image_max_dimension"`
	} `json:"media"`

	Logging struct {
		// RequestArchiveEnabled 请求元数据 JSONL 归档开关，缺省 true。
		RequestArchiveEnabled bool `json:"request_archive_enabled"`
		// RequestRetentionDays 归档保留天数，缺省 7；<=0 回落默认。
		RequestRetentionDays int `json:"request_retention_days"`
		// RequestArchiveMaxMB 归档总上限（MiB），缺省 100；<=0 回落默认。
		RequestArchiveMaxMB int `json:"request_archive_max_mb"`
		// RequestClientInfo 是否在请求日志（归档事件 + stdout 流水行 + 面板运行
		// 日志）里记录调用来源：客户端 IP 与 User-Agent。缺省 true。
		//
		// 为什么做成开关而不是恒开：来源信息是排查"谁在打网关"的第一手线索，
		// 但它比 token 计数敏感（IP 属个人信息），共享部署/多租户场景可能需要
		// 关掉。关闭后 Event.ClientIP/UserAgent 保持为空，归档里不出现该字段。
		// 热生效（经 config/runtime 快照），无需重启。
		RequestClientInfo bool `json:"request_client_info"`
	} `json:"logging"`

	Server struct {
		// MaxInflightRequests 服务级入站准入：并发「读取 + 整包解析 + 图片校验」的
		// 请求数上限。0 = 不限制该项。缺省 64。
		//
		// 为什么需要：账号租约（单账号在途上限）在选号之后才申请，而最贵的
		// io.ReadAll（最多 32 MiB）+ JSON 解析 + 图片 base64 校验发生在它之前——
		// 没有这道闸门时该阶段的内存放大与并发数成正比，与账号数无关。
		MaxInflightRequests int `json:"max_inflight_requests"`
		// MaxInflightBytesMB 同上，按 Content-Length 计的并发字节预算（MiB）。
		// 0 = 不限制该项。缺省 256。无 Content-Length（chunked）按 2 MiB 计入。
		MaxInflightBytesMB int `json:"max_inflight_bytes_mb"`
		// IngressWait 准入满载时的等待上限（如 "5s"；"0" = 立即拒绝）。
		// 等待期间不持有内存，只占连接；超时后回 503 server_busy。
		IngressWait string `json:"ingress_wait"`
		// ReadTimeout 入站请求读取（含 body 上传）总时长上限（issue #100）。
		// http.Server 的 ReadTimeout 覆盖整个请求读取：大上下文/文件块请求经
		// 反代链转发时上传可超过旧固定值 60s，被掐后客户端拿到
		// 400 "read body: ... i/o timeout"。缺省 "300s"；"0" = 不限制
		//（慢速 body 可无限占用连接，自担风险）；改动需重启进程。
		ReadTimeout string `json:"read_timeout"` // "300s"；"0" = 不限制
	} `json:"server"`

	Cooldown struct {
		// hard_credit / err_threshold / err_cooldown 三个历史键已退役：
		// 硬冷却固定为次日 04:00（CooldownUntilTomorrow4AM），连续错误语义并入熔断器。
		// 旧 config 中的这些键因 JSON 未知字段而自然忽略，不报错。
		SoftRate string `json:"soft_rate"` // "600s"，软限流冷却基数
		// SoftRateMax 软冷却指数退避的封顶，默认 "2h"。
		// 空值回落默认，非法值报错（处理风格同 soft_rate）。
		SoftRateMax string `json:"soft_rate_max"` // "2h"
	} `json:"cooldown"`

	Schedule struct {
		CheckinHours   []int `json:"checkin_hours"`   // [9,21]
		TravelHours    []int `json:"travel_hours"`    // [9,21]
		ActivityHours  []int `json:"activity_hours"`  // [10]
		KeepaliveHours []int `json:"keepalive_hours"` // [22]
		BlackcatHours  []int `json:"blackcat_hours"`  // [23] 夜猫子窗口（23:00–08:00 计数）
		GrowthHours    []int `json:"growth_hours"`    // [1] 成长任务队列（Sequential 族每日零点解锁，01:00 自动扫描执行）
		// CheckinEnabled/TravelEnabled/ActivityEnabled/KeepaliveEnabled/BlackcatEnabled 显式禁用开关（缺省 true）。
		//
		// 为什么用独立 bool 而不是空数组/哨兵值表意"禁用"：
		//   - 空数组与 null 在老语义里已被"未配置 → 回落默认"占用，改判会静默翻转
		//     所有老 config 的行为（用户只想删掉一行，结果关掉了签到）；bool 缺省 true
		//     则对老配置零影响，向后完全兼容。
		//   - 开关与取值解耦：禁用时仍保留用户显式配的小时，重新启用无需补配。
		//   - 无需猜测哨兵（[-1] 之类），非法小时一律报错并提示改用本开关。
		// 旧 config 里的该键因 JSON 未知字段而自然忽略，不报错。
		CheckinEnabled   bool `json:"checkin_enabled"`   // 缺省 true；false = 关签到
		TravelEnabled    bool `json:"travel_enabled"`    // 缺省 true；false = 完全停猫猫旅行
		ActivityEnabled  bool `json:"activity_enabled"`  // 缺省 true；false = 停活跃上报
		KeepaliveEnabled bool `json:"keepalive_enabled"` // 缺省 true；false = 关 token 保活
		BlackcatEnabled  bool `json:"blackcat_enabled"`  // 缺省 true；false = 关夜猫子
		GrowthEnabled    bool `json:"growth_enabled"`    // 缺省 true；false = 关成长任务自动排程

		// IncludeDisabledInTasks 让「保号类」定时任务（签到 / 活跃上报 / token 保活 /
		// 余额刷新）对**已禁用（disabled）**的账号也执行。
		//
		// 为什么需要它：面板「禁用」的语义是「不再参与选号」（见面板确认文案），但这四类
		// 任务此前一律 `if st.Disabled { continue }`，等于把「停用流量」放大成「停止一切
		// 上游保号行为」——被禁用的号拿不到签到积分、不续 token、余额也不再刷新；而
		// ReenableIfCredits 明确不复活 disabled 账号（见 pool.state.go），于是签到这条唯一
		// 的自动回血路径也断了，账号只能靠人工「解冻」回来。
		//
		// 对「一次只放开一个号、用禁用做流量开关」的轮换用法（同 IP 多号防风控），闲置
		// 待命的号恰恰是最需要签到的那批——本开关即为该用法提供出口。
		//
		// 缺省 false = 保持既有行为，对老配置零影响。打开后禁用号仍会签到 / 保活，但
		// **依旧不参与选号**：pool 选号侧的 disabled 过滤不受本开关影响。
		IncludeDisabledInTasks bool `json:"include_disabled_in_tasks"`

		// 余额后台周期刷新：两次签到时点之间 credits 也能保持新鲜（面板/状态观测用）。
		// 解冻语义同签到（余额 > 0 的冷却账号自动解冻），但不做签到不刷 token。
		BalanceRefreshEnabled bool `json:"balance_refresh_enabled"` // 缺省 true；false = 关闭
		BalanceRefreshMinutes int  `json:"balance_refresh_minutes"` // 缺省 5；<=0 回落 5

		// token 到期前兜底刷新（schedule.token_refresh_*）：周期性扫描全部账号，对剩余
		// 寿命小于 TokenRefreshWindow（24h，对齐官方客户端判据）的号续期。
		//
		// 与 keepalive_hours 的分工：keepalive 是「每天到点把所有号无差别续一遍」，
		// 本扫描是「只看即将过期的号」的兜底——keepalive 被关掉（或某号因轮换长期无流量）
		// 时，到期窗口内仍有入口给它们续期；客户端侧对应 24h 定时器。两着同时开启时，
		// 常态下本扫描零上游调用（号刚被 keepalive 刷过，剩余寿命远大于 24h）。
		TokenRefreshEnabled bool `json:"token_refresh_enabled"` // 缺省 true；false = 关闭
		TokenRefreshMinutes int  `json:"token_refresh_minutes"` // 缺省 10；<=0 回落 10
	} `json:"schedule"`

	// Growth 成长任务自动化策略（面板「任务中心」/账号「一键完成」/growth 排程共用）。
	//
	// 上线背景：哪些活动任务能被自动化，此前是 internal/panel 里的硬编码表
	//（autoActions / builtinMPTaskCodes）——上游新增活动只能改代码重编译。本段配置让
	// 「启用集合、执行顺序、mp 口径、逐任务参数」四类调整不改代码即可生效；
	// **判据事件的形状**（字段/埋点指纹）仍是代码（逆向产物），配置只能引用
	// 已实现的动作，不能用配置造出新判据。
	Growth struct {
		Autotasks GrowthAutotasks `json:"autotasks"`
	} `json:"growth"`

	Global struct {
		// Enabled global realm 路由开关。缺省 true：Realm() 正常把 realm=global/
		// domain=workbuddy.ai 的账号判为 global 并路由 global base/路径。
		// 显式 "enabled": false 关闭（逃生门，纯 CN 锁定：即便 auth 写了 realm=global
		// 也不路由，auth.Realm() 双保险的第一道闸）。纯 CN 部署行为不变：CN 账号
		// 恒判 cn，global base 只在 realm=global 的账号上被使用。
		Enabled bool `json:"enabled"`
		// ChatBase / BillingBase 国际版上游 base 覆盖；空 = 回落内置默认
		// https://www.workbuddy.ai（internal/upstream.defaultGlobalBase）。
		ChatBase    string `json:"chat_base"`
		BillingBase string `json:"billing_base"`
	} `json:"global"`

	// ModelDefaultRealm 裸模型名的默认域策略（无 cn:/global: 前缀时）：
	//   "cn"（默认，零回归）= 裸名归 CN；
	//   "global" = 裸名归国际版（纯 global 池免写前缀）；
	//   "auto"（或 "auto:cn,global"）= 自动判定（CN 优先，无号/失败时轮退至 global）；
	//   "auto:global,cn" = 自动判定（Global 优先，无号/失败时轮退至 cn）。
	// 显式 cn:/global: 前缀恒优先，不受本项影响。
	ModelDefaultRealm string `json:"model_default_realm"`

	Upstream struct {
		// TimeoutSeconds 短 RPC（refresh/checkin/balance/FetchModels）总时长上限，默认 120。
		TimeoutSeconds int `json:"timeout_seconds"`
		// HeaderTimeoutSeconds 聊天 SSE 首字节前（响应头）上限；<=0 回落 TimeoutSeconds。
		HeaderTimeoutSeconds int `json:"header_timeout_seconds"`
		// IdleTimeoutSeconds 聊天 SSE 流中空闲上限（活跃吐数据续命不掐）；<=0 回落默认 300。
		IdleTimeoutSeconds int                                 `json:"idle_timeout_seconds"`
		Profiles           map[string]upstream.IdentityProfile `json:"profiles"`
		ChatBaseCN         string                              `json:"chat_base_cn"`
		// SegmentTypes 与默认值：0 表示未设置。
		FirstModelEventSeconds int `json:"first_model_event_seconds"`
		FirstGenerationSeconds int `json:"first_generation_seconds"`
		TailSeconds            int `json:"tail_seconds"`
		// ClientName 用量归属头取值（X-Product / X-IDE-Name / X-IDE-Type / X-IDE-Version）。
		// 空 = 旧行为 X-Product="SaaS" 不设 X-IDE-*；配 "WorkBuddy" 则四头跟随。
		ClientName string `json:"client_name"`
		// DeviceToken 设备风控 Token（X-Device-Token 头）全局兜底；空 = 不注入。
		// 每号 auth 文件的 device_token 键优先于本项。
		DeviceToken string `json:"device_token"`
		// DeviceTokenFile device token 文件路径兜底（宿主落盘的桌面端 token，5 分钟读取缓存）。
		DeviceTokenFile string `json:"device_token_file"`
		// PassthroughIP 是否透传客户端 IP 给上游（默认 false，反代安全边界）。
		PassthroughIP bool `json:"passthrough_ip"`
	} `json:"upstream"`

	Prompt struct {
		// Mode 组合位置（**默认 inject**）：
		//
		//	inject  客户端 system 套官方 <user_custom_instructions> 包装并入网关正文
		//	        （缺省；保留客户端规则，同时不让第三方 CLI 指纹原样上流）
		//	none    显式透传：不改写客户端 system/developer（需手动选）
		//	replace 删除全部 system/developer，只留网关提示词（指纹面最小）
		//	append  客户端开头 system/developer 块之后插网关提示词（客户端在前）
		//	after   网关提示词置首，客户端开头块紧随其后（后组合，客户端内容不丢）
		//
		// 空串 = inject（不是 none）；其余取值一律报错（无旧别名兼容）。
		Mode string `json:"mode"`
		// Preset 内置预设名（面板下拉由 prompt.Presets() 生成，前端不硬编码）：
		// official-craft / official-ask / official-plan / official-quick / official-expert，
		// 分域取 <name>.<realm>.md（缺失回落 <name>.md）。
		// 空 = official-craft。预设正文是**官方渲染产物逐字**（无模板标记、无变量）。
		Preset string `json:"preset"`
		// File 提示词文件路径；非空且不可读 → 启动/保存报错（fail fast）。
		// 优先级低于 Text、高于 Preset。
		File string `json:"file"`
		// Text 内联提示词正文（面板可直编）。非空优先于 File/Preset——
		// “直接把内容写进配置”的入口，无需额外落盘文件。
		Text string `json:"text"`
		// Profiles 按账号域覆盖上列各项（键：cn / global）。mode 逐项回落到顶层；
		// 素材（preset/file/text）为整体覆盖（任一非空则本域完全
		// 用自己那三项）。用于“同一网关同时对 CN 与 Global 账号使用不同提示词”。
		Profiles map[string]PromptProfile `json:"profiles,omitempty"`
	} `json:"prompt"`

	// PromptText 默认域解析后的系统提示词文本（历史字段，保留兼容）。
	// 等价于 PromptRules[""] 的 Text；新代码请读 PromptRules。
	PromptText string `json:"-"`

	// PromptRules 各域生效的提示词规则（键："" 默认、"cn"、"global"）。
	// normalize 阶段构建；保存配置时整体 Store 进 handler 的 prompt.Holder（热生效），
	// handler 按请求 realm 从快照选规则。
	PromptRules map[string]prompt.Rule `json:"-"`

	// Warnings 载入期的配置告警（未知键、被忽略的非法项）。运行期元数据：
	// 不从文件读入、也不落盘（保存前显式清空），供启动日志与面板 `_warnings` 展示。
	Warnings []string `json:"_warnings,omitempty"`

	Upstash struct {
		URL   string `json:"url"`   // 空 = 纯内存模式；支持完整 rediss:// URL 或 https://xxx.upstash.io host
		Token string `json:"token"` // url 非完整连接串时用于组装 rediss://default:<token>@<host>:6379
	} `json:"upstash"`

	Pool struct {
		MaxInFlight        int    `json:"max_in_flight"`        // 单账号最大在途请求数，0 = 不限
		MaxInFlightGlobal  int    `json:"max_in_flight_global"` // global 域单账号在途上限（WAF 风控紧域压低并发），0 = 回落默认 2
		BreakerThreshold   int    `json:"breaker_threshold"`    // 连续失败次数触发熔断，默认 3
		BreakerCooldown    string `json:"breaker_cooldown"`     // 基础熔断时长，默认 "30m"
		BreakerCooldownMax string `json:"breaker_cooldown_max"` // 指数退避封顶，默认 "6h"
		// 连败降权（issue #114）：ErrClient/传输层这类「不罚号」失败连续计数，达阈
		// 临时出池。与冷却/熔断并存取更长者不叠加。默认 5 次 / 10m。
		DegradeThreshold   int     `json:"degrade_threshold"`    // 连败次数触发降权，默认 5
		DegradeCooldown    string  `json:"degrade_cooldown"`     // 降权时长（固定，非指数退避），默认 "10m"
		DegradeCooldownMax string  `json:"degrade_cooldown_max"` // 降权时长的上限钳制，默认 "2h"（仅当 cooldown 超该值才钳制）
		IdleWeightPerHour  float64 `json:"idle_weight_per_hour"` // 闲置补偿：每小时未用 +0.5 权重
		IdleWeightMax      float64 `json:"idle_weight_max"`      // 闲置补偿封顶，默认 5.0
		// PreferExpiring 快过期积分加权开关，默认 true。开启且 expiring_soon 窗口内
		// 存在有效批次时，该账号选号权重 ×3（虚拟实例，见 pool 路由加权）；
		// 不按到期时间排序、与批次金额无关（issue #101 对齐实现口径）。
		// 关闭后完全不使用到期信息选号。
		PreferExpiring bool `json:"prefer_expiring"`
		// ExpiringSoon 快过期积分窗口（如 "168h"=7天）：签到/余额刷新时，到期时间在
		// 此窗口内的批次令账号命中上述 ×3 加权；窗口开大 → 命中账号变多、
		// 偏好被稀释。空/0 = 禁用该加权门槛。
		ExpiringSoon string `json:"expiring_soon"`
		// CostExploreInterval costTier 条件探索窗口（issue #136 方案 a′）：tier 0
		// 垄断层存在且 tier 1 有成员时，距上次探索 ≥ 窗口则本次 pick 生效层切
		// tier 1-only（探索=搭车改道，零新增上游请求；成功即毕业，失败走既有
		// 错误策略）。默认 "30m"（≤48 次/天/模型）；"0" 关停（完全回到现状行为）；
		// 空值回落默认。
		CostExploreInterval string `json:"cost_explore_interval"`
		// CreditFloor 积分保底：账号余额低于该值时，对实测收费模型（tier 2）不再
		// 参与选号——防止收费请求把余额打穿、连免费模型都 402 冷却到次日签到。
		// tier 0（免费）/ tier 1（无观测）不受限；签到回血越过 floor 自动恢复。
		// 默认 0 = 关闭；负值钳 0。
		CreditFloor int64 `json:"credit_floor"`
	} `json:"pool"`

	SessionSticky struct {
		Enabled    bool   `json:"enabled"`     // 默认 true
		TTL        string `json:"ttl"`         // 会话绑定 TTL，默认 "30m"
		GCInterval string `json:"gc_interval"` // 会话 GC 周期，默认 "5m"
		// FingerprintEnabled 派生键指纹救场（默认 true）：派生键粘性未命中时，
		// 用 user/assistant 历史窗口匹配找回该会话上一跳的账号（仅派生键会话，
		// 显式会话键不受影响）。救场材料纯内存，不落盘不进上游。
		FingerprintEnabled bool `json:"fingerprint_enabled"`
	} `json:"session_sticky"`

	// 出站代理接入（低侵入，见 internal/proxy 与 FORK_CHANGES.md）。
	// 普通正向代理（proxy_url）与 Resin（resin_url）互斥，空 = 未接入，
	// 出站行为与主线逐字一致。账号可用 use_proxy 单独关闭代理（默认开）。
	//
	// ProxyURL 普通正向代理地址：http/https/socks5/socks5h，可含 userinfo 凭证，
	// 如 http://user:pass@127.0.0.1:8080 或 socks5://127.0.0.1:1080。
	ProxyURL string `json:"proxy_url"`
	// Resin 外部粘性代理池：
	ResinURL          string `json:"resin_url"`           // 含基址与 Token，如 http://127.0.0.1:2260/my-token
	ResinPlatformName string `json:"resin_platform_name"` // Platform 字段，必须与部署一致
	ResinMode         string `json:"resin_mode"`          // reverse（默认，推荐）/ forward
	ResinAuthVersion  string `json:"resin_auth_version"`  // V1（默认）/ LEGACY_V0，仅正向代理使用

	// 解析后
	SoftRateDur         time.Duration `json:"-"`
	SoftRateMaxDur      time.Duration `json:"-"`
	BreakerCooldownDur  time.Duration `json:"-"`
	BreakerCooldownMaxD time.Duration `json:"-"`
	DegradeCooldownDur  time.Duration `json:"-"`
	DegradeCooldownMaxD time.Duration `json:"-"`
	SessionTTL          time.Duration `json:"-"`
	SessionGCInterval   time.Duration `json:"-"`
	// SessionFingerprint 指纹救场开关（session_sticky.fingerprint_enabled，
	// normalize 阶段从 SessionSticky.FingerprintEnabled 携带；main 装配与
	// 热改路径只拿归一化后的标量）。
	SessionFingerprint     bool          `json:"-"`
	BalanceRefreshInterval time.Duration `json:"-"` // 0 = 不启动（enabled=false）
	// TokenRefreshInterval 解析后的到期前兜底扫描间隔（schedule.token_refresh_*）；
	// 0 = 不启动（enabled=false）。
	TokenRefreshInterval time.Duration `json:"-"`
	ExpiringSoonDur      time.Duration `json:"-"`
	// CostExploreIntervalDur 解析后的 costTier 探索窗口（issue #136）；0 = 关停。
	CostExploreIntervalDur time.Duration `json:"-"`
	// ServerReadTimeoutDur 解析后的入站请求读取上限（issue #100）；0 = 不限制。
	ServerReadTimeoutDur time.Duration `json:"-"`
	// IngressWaitDur 解析后的入站准入等待上限（server.ingress_wait）；0 = 满载立即拒绝。
	IngressWaitDur time.Duration `json:"-"`
	// ProxyClient 解析校验后的代理接入实例（普通代理或 Resin）；nil = 未接入
	// （JSON 不序列化）。
	ProxyClient *proxy.Client `json:"-"`
}

// GrowthAutotasks 成长任务自动化策略（growth.autotasks）。
//
// 语义要点：
//   - 缺省（全空）= 既有行为：内置动作表全部启用、内置 mp 码表生效、内置参数。
//   - disabled / only 只影响「自动化动作」的可用集合，不影响账号页的任务列表
//     展示，也不影响手动「接受 / 领取」接口。
//   - order 只影响执行顺序（队列与一键完成），不改变动作本身的幂等语义。
//   - tasks 的参数覆盖只作用于已实现动作；未实现判据的任务只能通过
//     allow_unknown_claim 做 accept + 达标领奖。
//
// 本段全部为热生效字段（保存即生效，无需重启）。
// 完整文件示例与字段速查见 config.example.json / README。
type GrowthAutotasks struct {
	// Disabled 屏蔽的任务 code 黑名单（trim 后精确匹配，大小写敏感）。
	// 缺省空 = 不屏蔽任何内置动作。
	Disabled []string `json:"disabled,omitempty"`
	// Only 白名单：非空时只有列出的 code 参与自动化；同时命中 Disabled 时
	// Disabled 优先（= 屏蔽）。缺省空 = 不启用白名单。
	Only []string `json:"only,omitempty"`
	// Order 执行顺序覆盖：列出的 code 按本列表次序执行，未列出的排在其后
	//（按内置依赖序）。缺省空 = 完全按内置序（先解锁依赖项）。
	Order []string `json:"order,omitempty"`
	// MPCodes 小程序口径专属任务码：**裸值 = 整体覆盖**内置表，前缀 "+" =
	// 追加到内置表。运行时由「默认列表 vs mp 列表」差集探测到的码始终并入，
	// 因此只有探测不到的场景（列表失败/任务刚上线）才需要在此登记。
	MPCodes []string `json:"mp_codes,omitempty"`
	// AllowUnknownClaim 对「上游已下发但无判据动作实现」的任务只做
	// accept + 达标领奖（绝不伪造事件）；缺省 false = 未实现的任务不参与自动化。
	AllowUnknownClaim bool `json:"allow_unknown_claim"`
	// Tasks 逐任务参数覆盖（键 = task_code）。缺省空 = 全部用内置默认。
	Tasks map[string]GrowthTaskPolicy `json:"tasks,omitempty"`
}

// GrowthTaskPolicy 单个任务的自动化参数覆盖（growth.autotasks.tasks.<code>）。
// 每一项留空 / 零值 = 用内置默认（上游下发或代码常量）。
type GrowthTaskPolicy struct {
	// ActionType 动作模板类型：
	//   - "" / "builtin"：使用内置硬编码动作（未内置且无模板时回落既有行为）；
	//   - "chat_count"：通用对话活跃上报模板（自动补足差额）；
	//   - "model_chat"：通用指定模型真实对话打卡模板（发一次对话 + 模型活跃上报）；
	//   - "mp_chat"：通用小程序对话上报模板（带 mp 头与 source 埋点）；
	//   - "claim_only"：仅接受 + 达标自动领奖（不伪造任何事件）。
	ActionType string `json:"action_type,omitempty"`
	// Model 当 ActionType 为 "model_chat" 时指定模型 ID（如 "glm-5.2"）；空 = 缺省 "glm-5.2"。
	Model string `json:"model,omitempty"`
	// Gap 判据事件之间的间隔（Go 时长语法，如 "45s"）。作用于有上游反作弊
	// 校验的连发任务（mp 对话事件、chat_5 等）；空 = 内置默认。
	Gap string `json:"gap"`
	// Target 进度目标覆盖；0 = 以上游下发为准（推荐，任务改版后自动跟随）。
	// 注意：设成**低于**上游下发值会在进度未足时就去领奖，上游回 400
	// "task not completed"（日志可见）；需要降目标时应确认上游确实不会再累加。
	Target int64 `json:"target"`
	// Attempt 覆盖「尝试型」标记（仅影响面板/接口的 attempt 提示，不改变执行逻辑）；
	// null / 缺省 = 内置值。
	Attempt *bool `json:"attempt"`
	// Window 计数窗口 "HH:MM-HH:MM"（支持跨零点，如 "23:00-08:00"）。
	// 目前由 black_cat 使用；空 = 内置 23:00-08:00。
	Window string `json:"window"`
	// ActivityID 上报事件的 activityId 覆盖（目前由 school_season 使用）；
	// 空 = 内置 school_open_day_2026。
	ActivityID string `json:"activity_id"`

	// ── 派生（normalize 解析，非法值在启动/保存时报错，不落盘）──
	GapDur         time.Duration `json:"-"`
	WindowStartMin int           `json:"-"` // 窗口起（当日分钟数，0-1439）
	WindowEndMin   int           `json:"-"` // 窗口止（当日分钟数，0-1439）
	HasWindow      bool          `json:"-"` // Window 非空（跨零点由 start>end 表意）
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:    ":7863",
		APIKey:    "",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
	}
	c.Cooldown.SoftRate = "600s"
	c.Cooldown.SoftRateMax = "2h"
	c.Server.ReadTimeout = "300s"
	c.Server.MaxInflightRequests = defaultMaxInflightRequests
	c.Server.MaxInflightBytesMB = defaultMaxInflightBytesMB
	c.Server.IngressWait = "5s"
	c.Panel.PackageDetailLimit = 5
	// 工具结果图片策略默认 auto（原生 Chat 透传 / 桥接抬升）。显式写成 auto 而不是留
	// 空串，是为了面板下拉有匹配项：空串在 <select> 里匹配不上任何 option，回显会是
	// 空白（配置与表单显示漂移）。空串仍是合法别名（兼容手写配置）。
	c.Media.ToolImages = "auto"
	c.Logging.RequestArchiveEnabled = true
	c.Logging.RequestRetentionDays = 7
	c.Logging.RequestArchiveMaxMB = 100
	// 缺省 true 靠显式赋值实现（同 Schedule 开关）：JSON 里键缺席时字段保留此值，
	// 只有显式 false 才关闭来源记录。
	c.Logging.RequestClientInfo = true
	c.Schedule.CheckinHours = []int{9, 21}
	c.Schedule.TravelHours = []int{9, 21}
	c.Schedule.ActivityHours = []int{10}
	c.Schedule.KeepaliveHours = []int{22}
	c.Schedule.BlackcatHours = []int{23}
	c.Schedule.GrowthHours = []int{1}
	// 开关「缺省 true」靠这几行实现：Load 先取 Default() 再 json.Unmarshal 覆盖，
	// 键缺席（或为 null）时字段原样保留 true，只有显式 false 才关。
	c.Schedule.CheckinEnabled = true
	c.Schedule.GrowthEnabled = true
	c.Schedule.TravelEnabled = true
	c.Schedule.ActivityEnabled = true
	c.Schedule.KeepaliveEnabled = true
	c.Schedule.BlackcatEnabled = true
	c.Schedule.BalanceRefreshEnabled = true
	c.Schedule.BalanceRefreshMinutes = 5
	c.Schedule.TokenRefreshEnabled = true
	c.Schedule.TokenRefreshMinutes = 10
	c.Upstream.TimeoutSeconds = 120
	// HeaderTimeoutSeconds/IdleTimeoutSeconds 默认 0（未设置态），回落见 normalize()。
	c.Upstream.HeaderTimeoutSeconds = 0
	c.Upstream.IdleTimeoutSeconds = 0
	// Global.Enabled 缺省 true（纯 CN 行为不变：CN 账号恒判 cn，global base 不被使用）；
	// ChatBase/BillingBase 缺省空（回落内置默认）。
	c.Global.Enabled = true
	c.ConfigVersion = CurrentVersion
	c.Upstream.FirstModelEventSeconds = 120
	c.Upstream.FirstGenerationSeconds = 300
	c.Upstream.TailSeconds = 10
	c.Upstream.Profiles = map[string]upstream.IdentityProfile{"cn": upstream.DefaultIdentity("cn"), "global": upstream.DefaultIdentity("global")}
	c.Upstream.ChatBaseCN = "https://www.workbuddy.cn"
	c.Prompt.Mode = "none"
	c.Pool.MaxInFlight = 3
	// MaxInFlightGlobal 缺省 2：global 域 WAF 风控更紧，压低单号并发（WAF 403 修复
	// P1-1）；0/负数 normalize 回落默认（与 max_in_flight 的 0=不限语义不同，分档键
	// 的 0 没有合理语义，回退分档默认最稳）。
	c.Pool.MaxInFlightGlobal = 2
	c.Pool.BreakerThreshold = 3
	c.Pool.BreakerCooldown = "30m"
	c.Pool.BreakerCooldownMax = "6h"
	c.Pool.DegradeThreshold = 5
	c.Pool.DegradeCooldown = "10m"
	c.Pool.DegradeCooldownMax = "2h"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5.0
	c.Pool.PreferExpiring = true
	c.Pool.ExpiringSoon = "168h" // 快过期窗口默认 7 天：官方活动奖励积分多在两周内过期
	// costTier 探索默认 30m（issue #136：垄断破除 + 搭车改道零新增请求）；"0" 关停。
	c.Pool.CostExploreInterval = "30m"
	c.SessionSticky.Enabled = true
	c.SessionSticky.TTL = "30m"
	c.SessionSticky.GCInterval = "5m"
	c.SessionSticky.FingerprintEnabled = true
	// Resin 缺省未接入：URL/Platform 空，模式与认证版本给推荐值便于展示/落盘。
	c.ResinMode = "reverse"
	c.ResinAuthVersion = "V1"
	// 裸名默认域：缺省 cn（历史行为零回归）。
	c.ModelDefaultRealm = "cn"
	return c
}
