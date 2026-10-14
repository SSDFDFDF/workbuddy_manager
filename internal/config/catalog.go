// catalog.go 字段目录：每个配置字段「改了要不要重启」的唯一真相。
//
// 为什么需要它：在引入本文件之前，"哪些字段热生效 / 哪些需重启"分别写在
// 三处——saveConfig 的热应用代码、restartRequiredFields 的手写清单、
// 面板 index.html 手写的「需重启」徽标。三者已经漂移（例如
// session_sticky.enabled 既没进清单也没徽标：面板上改它显示"已保存"，
// 实际零效果且无任何提示）。
//
// 现在只有这里一份：
//   - 保存响应：DiffConfig 算出的实际改动 ∩ Restart 字段 → 面板提示；
//   - HTTP：GET /panel/api/config/catalog → 面板按字段自动渲染徽标与原因；
//   - 守护测试：TestCatalogCoversEveryConfigLeaf 保证 Config 的每个叶子都有登记，
//     TestCatalogModesConsistent 保证通配项之间不打架。
//
// 新增配置字段时**必须**在这里登记：TestCatalogCoversEveryConfigLeaf 会拦住漏登记。
package config

import (
	"encoding/json"
	"sort"
	"strings"
)

// Mode 字段的生效方式。
type Mode uint8

const (
	// Hot 保存即生效：saveConfig 里有对应的热应用动作，无需重启。
	Hot Mode = iota
	// Restart 必须重启进程：字段被装配期对象捕获（监听地址、出站 client、
	// 文件句柄、目录扫描、连接池……），进程内没有安全的替换点。
	Restart
)

// String 供日志与 API 使用（JSON 里也走它，见 MarshalJSON）。
func (m Mode) String() string {
	if m == Restart {
		return "restart"
	}
	return "hot"
}

// MarshalJSON 让 API 返回 "hot"/"restart" 而不是数字：面板与第三方脚本
// 读的是语义，不是枚举序号（序号会随新增枚举漂移）。
func (m Mode) MarshalJSON() ([]byte, error) { return json.Marshal(m.String()) }

// Field 单个配置字段的元数据。
type Field struct {
	// Path 点分路径。末尾的 ".*" 表示"该子树下的全部叶子"（见 MatchPath）。
	Path string `json:"path"`
	// Mode 热生效 / 需重启。
	Mode Mode `json:"mode"`
	// Passive 为真表示 Hot 但无需任何 apply 动作：值在读侧按需读取
	// （例：panel.package_detail_limit 由面板每次 GET 配置时现读）。
	// 它的意义是让 TestApplyClaimsEveryHotField 不要求一个不存在的 applier。
	Passive bool `json:"passive,omitempty"`
	// Why Mode==Restart 的原因：面板把它作为徽标的 title 展示，保存响应里
	// 也用它解释"为什么这项要重启"。Hot 项留空。
	Why string `json:"why,omitempty"`
	// Group 面板分组（base/upstream/realm/prompt/pool/schedule/session/
	// ingress/traffic/proxy/storage），仅用于展示与文档归类。
	Group string `json:"group,omitempty"`
}

// Catalog 字段目录（不可变快照，只读）。
type Catalog []Field

// Lookup 返回覆盖 path 的目录项。path 为具体叶子路径（DiffConfig / LeafPaths
// 的产物），模式里的 "*" 只出现在目录项一侧。
func (c Catalog) Lookup(path string) (Field, bool) {
	for _, f := range c {
		if MatchPath(f.Path, path) {
			return f, true
		}
	}
	return Field{}, false
}

// LookupAll 返回全部覆盖 path 的目录项（守护测试用：检查是否存在语义冲突）。
func (c Catalog) LookupAll(path string) []Field {
	var out []Field
	for _, f := range c {
		if MatchPath(f.Path, path) {
			out = append(out, f)
		}
	}
	return out
}

// MatchPath 报告目录模式 pattern 是否覆盖具体路径 path。
//
// 唯一的通配语义：末尾 ".*" = 该子树下的**全部叶子（递归）**，不含节点自身。
//
//	MatchPath("pool.*", "pool.max_in_flight")           → true
//	MatchPath("pool.*", "pool.breaker.cooldown")        → true（递归）
//	MatchPath("upstream.profiles.*", "upstream.profiles.cn.client_version") → true
//	MatchPath("upstream.profiles.*", "upstream.profiles")                   → false（节点自身不是叶子）
//	MatchPath("listen", "listen")                       → true
//	MatchPath("listen", "listen_extra")                 → false（不做前缀匹配）
func MatchPath(pattern, path string) bool {
	if prefix, ok := strings.CutSuffix(pattern, ".*"); ok {
		return strings.HasPrefix(path, prefix+".")
	}
	return pattern == path
}

// entries 目录全量数据。
//
// 口径：**如实反映当前实现**，不写"目标态"。已热化的字段在这里标 Hot，并在
// cmd/server 的热应用里接上动作——TestApplyClaimsEveryHotField 会在两者不一致时
// 失败。这样"面板说即时生效"永远等于"真的即时生效"。
//
// 仍为 Restart 的只有 5 项：listen / state_file / server.read_timeout /
// upstash.url / upstash.token——共同点是绑定监听套接字、派生持久化路径、或
// 写进被并发读取的共享对象，没有安全的进程内替换点（Why 逐项写明）。
//
// upstream.header_timeout_seconds 走「重建 Transport + 原子换客户端」（连接池
// 重置、值未变则空操作），因此不在 Restart 之列。
var entries = Catalog{
	// ── 基础 ────────────────────────────────────────────────────────────
	// config_version：程序写文件时固定写 CurrentVersion，读取时不做版本转换
	//（只拒绝比程序新的版本），用户无需维护，也没有热应用动作。
	{Path: "config_version", Mode: Hot, Passive: true, Group: "base"},
	{Path: "listen", Mode: Restart, Group: "base",
		Why: "端口在装配期由 http.Server.Addr 绑定，热换需要起第二个 listener 并排空旧连接"},
	{Path: "api_key", Mode: Hot, Group: "base",
		Why: ""},
	// auth_dir：保存时重扫目录并把账号池对齐（auth.LoadDir + pool.SyncToDir），
	// 面板登录/导入的落盘目录同步切换；路径本身不在装配期被任何对象捕获。
	{Path: "auth_dir", Mode: Hot, Group: "base",
		Why: ""},
	{Path: "state_file", Mode: Restart, Group: "base",
		Why: "状态文件路径派生出用量/请求归档/模型缓存/cache-secret 等多个数据文件，中途换路径会分裂持久化视图"},
	{Path: "panel.package_detail_limit", Mode: Hot, Passive: true, Group: "base",
		Why: ""},

	// ── 指纹与媒体 ──────────────────────────────────────────────────────
	{Path: "fingerprint_rewrite", Mode: Hot, Group: "traffic", Why: ""},
	{Path: "fingerprint_disabled", Mode: Hot, Group: "traffic", Why: ""},
	{Path: "fingerprint_overrides.*", Mode: Hot, Group: "traffic", Why: ""},
	{Path: "fingerprint_rules", Mode: Hot, Group: "traffic", Why: ""},
	{Path: "media.*", Mode: Hot, Group: "traffic", Why: ""},

	// ── 日志与归档 ──────────────────────────────────────────────────────
	{Path: "logging.request_client_info", Mode: Hot, Group: "base", Why: ""},
	{Path: "logging.request_archive_enabled", Mode: Hot, Group: "base", Why: ""},
	{Path: "logging.request_retention_days", Mode: Hot, Group: "base", Why: ""},
	{Path: "logging.request_archive_max_mb", Mode: Hot, Group: "base", Why: ""},

	// ── 入站准入与读取 ──────────────────────────────────────────────────
	{Path: "server.max_inflight_requests", Mode: Hot, Group: "ingress", Why: ""},
	{Path: "server.max_inflight_bytes_mb", Mode: Hot, Group: "ingress", Why: ""},
	{Path: "server.ingress_wait", Mode: Hot, Group: "ingress", Why: ""},
	{Path: "server.read_timeout", Mode: Restart, Group: "ingress",
		Why: "http.Server.ReadTimeout 在 Serve 后被并发读取，进程内裸赋值有数据竞争（无安全的原地替换点）"},

	// ── 冷却与池 ────────────────────────────────────────────────────────
	{Path: "cooldown.*", Mode: Hot, Group: "pool", Why: ""},
	{Path: "pool.*", Mode: Hot, Group: "pool", Why: ""},

	// ── 排程 ────────────────────────────────────────────────────────────
	{Path: "schedule.*", Mode: Hot, Group: "schedule", Why: ""},

	// ── 成长任务自动化 ────────────────────────────────────────────────
	// growth.autotasks 全部热生效：保存时整体替换面板侧的策略快照（启用集合 /
	// 顺序 / mp 码 / 逐任务参数），任务中心与一键完成下一次动作即用新策略。
	{Path: "growth.*", Mode: Hot, Group: "growth", Why: ""},

	// ── 域策略 ──────────────────────────────────────────────────────────
	{Path: "global.enabled", Mode: Hot, Group: "realm", Why: ""},
	{Path: "global.chat_base", Mode: Hot, Group: "realm", Why: ""},
	{Path: "global.billing_base", Mode: Hot, Group: "realm", Why: ""},
	{Path: "model_default_realm", Mode: Hot, Group: "realm", Why: ""},

	// ── 提示词 ──────────────────────────────────────────────────────────
	{Path: "prompt.*", Mode: Hot, Group: "prompt", Why: ""},

	// ── 上游与出站身份 ──────────────────────────────────────────────────
	{Path: "upstream.profiles.*", Mode: Hot, Group: "upstream", Why: ""},
	{Path: "upstream.client_name", Mode: Hot, Group: "upstream", Why: ""},
	{Path: "upstream.device_token", Mode: Hot, Group: "upstream", Why: ""},
	{Path: "upstream.device_token_file", Mode: Hot, Group: "upstream", Why: ""},
	{Path: "upstream.passthrough_ip", Mode: Hot, Group: "upstream", Why: ""},
	{Path: "upstream.derive_conversation_id", Mode: Hot, Group: "upstream", Why: ""},
	{Path: "upstream.replay_thinking", Mode: Hot, Group: "upstream", Why: ""},
	{Path: "upstream.chat_base_cn", Mode: Hot, Group: "upstream", Why: ""},
	{Path: "upstream.timeout_seconds", Mode: Hot, Group: "upstream", Why: ""},
	{Path: "upstream.header_timeout_seconds", Mode: Hot, Group: "upstream", Why: ""},
	{Path: "upstream.idle_timeout_seconds", Mode: Hot, Group: "upstream", Why: ""},
	{Path: "upstream.first_model_event_seconds", Mode: Hot, Group: "upstream", Why: ""},
	{Path: "upstream.first_generation_seconds", Mode: Hot, Group: "upstream", Why: ""},
	{Path: "upstream.tail_seconds", Mode: Hot, Group: "upstream", Why: ""},
	// 模型默认思考档本地覆盖：进程级快照整体替换（SetEffortDefaultOverrides），
	// 下一次 /v1/models 与下一个请求即用新表。
	{Path: "upstream.effort_default_overrides.*", Mode: Hot, Group: "upstream", Why: ""},

	// ── 会话粘性 ────────────────────────────────────────────────────────
	{Path: "session_sticky.*", Mode: Hot, Group: "session", Why: ""},

	// ── 出站代理 ────────────────────────────────────────────────────────
	{Path: "proxy_url", Mode: Hot, Group: "proxy", Why: ""},
	{Path: "resin_url", Mode: Hot, Group: "proxy", Why: ""},
	{Path: "resin_platform_name", Mode: Hot, Group: "proxy", Why: ""},
	{Path: "resin_mode", Mode: Hot, Group: "proxy", Why: ""},
	{Path: "resin_auth_version", Mode: Hot, Group: "proxy", Why: ""},

	// ── 持久化镜像 ──────────────────────────────────────────────────────
	{Path: "upstash.url", Mode: Restart, Group: "storage",
		Why: "redisstore 连接在装配期建立，且粘性会话镜像依赖它"},
	{Path: "upstash.token", Mode: Restart, Group: "storage",
		Why: "同上：redisstore 连接在装配期建立"},
}

// nonUserLeaves 不是"用户可配置项"的叶子，不要求目录登记，附理由。
//
// 与 json:"-" 的派生态不同：这些字段**有 json tag**（面板读取它们展示），
// 但语义上不是可调配置，也不从文件读入（见 load.go 的 parseObject）。
// 守护测试要求每个豁免都写明原因（防止拿豁免绕过漏登记）。
var nonUserLeaves = map[string]string{
	"_warnings": "载入期告警的运行时载体（面板只读展示），不是可配置项",
}

// Entries 返回目录全量（按 Path 稳定排序的副本，调用方可安全持有）。
func Entries() Catalog {
	out := make(Catalog, len(entries))
	copy(out, entries)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// IsRestartPath 报告具体路径是否落在某个 Restart 目录项内。
func (c Catalog) IsRestartPath(path string) bool {
	f, ok := c.Lookup(path)
	return ok && f.Mode == Restart
}

// ConfigLeafPaths 返回 Config 的全部叶子路径模式（map 的键位用 "*"），
// 供守护测试核对目录覆盖率。排序去重。
func ConfigLeafPaths() []string {
	var out []string
	leafPatterns(cfgType(), "", &out)
	sort.Strings(out)
	return out
}
