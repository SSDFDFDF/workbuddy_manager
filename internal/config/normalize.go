// normalize.go 配置归一化与校验：所有「认识的键、值非法」都在这里 fail fast。
//
// 这里只做三件事：缺省值补齐、非法值钳制/报错、派生字段（*Dur / *Client /
// PromptRules / Window 分钟数）计算。
//
// 没有"为了兼容旧配置"的分支：不认识的键在读侧就已被忽略，认识的键取值非法
// 一律 fail fast（报错信息里给出合法取值），不静默降级成另一种行为。
//
// 未知键不进这里（只由 keyshape.go 告警）。
package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"workbuddy_manager/internal/media"
	"workbuddy_manager/internal/proxy"
	"workbuddy_manager/internal/scrub"
)

func (c *Config) normalize() error {
	var err error
	if c.Panel.PackageDetailLimit <= 0 {
		c.Panel.PackageDetailLimit = 5
	}
	if c.Logging.RequestRetentionDays <= 0 {
		c.Logging.RequestRetentionDays = 7
	}
	if c.Logging.RequestArchiveMaxMB <= 0 {
		c.Logging.RequestArchiveMaxMB = 100
	}
	// 入站读取上限（issue #100）：空值回落默认 300s；"0" 合法（不限制）；
	// 负值无语义，fail fast（静默钳 0 会把保护悄悄关掉）。
	if c.Server.ReadTimeout == "" {
		c.Server.ReadTimeout = "300s"
	}
	if c.ServerReadTimeoutDur, err = time.ParseDuration(c.Server.ReadTimeout); err != nil {
		return fmt.Errorf("server.read_timeout: %w", err)
	}
	if c.ServerReadTimeoutDur < 0 {
		return fmt.Errorf("server.read_timeout: 负时长 %q 无意义", c.Server.ReadTimeout)
	}
	// 入站准入（server.max_inflight_requests / max_inflight_bytes_mb / ingress_wait）：
	// 0 是合法值（= 该项不限制）；负值无语义，fail fast——静默钳 0 会把保护悄悄
	// 关掉，与 read_timeout 的处理风格一致。
	if c.Server.MaxInflightRequests < 0 {
		return fmt.Errorf("server.max_inflight_requests: 负值 %d 无意义（0 = 不限制）", c.Server.MaxInflightRequests)
	}
	if c.Server.MaxInflightBytesMB < 0 {
		return fmt.Errorf("server.max_inflight_bytes_mb: 负值 %d 无意义（0 = 不限制）", c.Server.MaxInflightBytesMB)
	}
	if c.Server.IngressWait == "" {
		c.Server.IngressWait = "5s"
	}
	if c.IngressWaitDur, err = time.ParseDuration(c.Server.IngressWait); err != nil {
		return fmt.Errorf("server.ingress_wait: %w", err)
	}
	if c.IngressWaitDur < 0 {
		return fmt.Errorf("server.ingress_wait: 负时长 %q 无意义（\"0\" = 满载立即拒绝）", c.Server.IngressWait)
	}
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	// 空值回落默认 2h（Default() 已置值；此兜底覆盖显式 "" 与 Default() 被绕过的场景）。
	if c.Cooldown.SoftRateMax == "" {
		c.Cooldown.SoftRateMax = "2h"
	}
	if c.SoftRateMaxDur, err = time.ParseDuration(c.Cooldown.SoftRateMax); err != nil {
		return fmt.Errorf("cooldown.soft_rate_max: %w", err)
	}
	if c.BreakerCooldownDur, err = time.ParseDuration(c.Pool.BreakerCooldown); err != nil {
		return fmt.Errorf("pool.breaker_cooldown: %w", err)
	}
	if c.BreakerCooldownMaxD, err = time.ParseDuration(c.Pool.BreakerCooldownMax); err != nil {
		return fmt.Errorf("pool.breaker_cooldown_max: %w", err)
	}
	if c.DegradeCooldownDur, err = time.ParseDuration(c.Pool.DegradeCooldown); err != nil {
		return fmt.Errorf("pool.degrade_cooldown: %w", err)
	}
	if c.DegradeCooldownMaxD, err = time.ParseDuration(c.Pool.DegradeCooldownMax); err != nil {
		return fmt.Errorf("pool.degrade_cooldown_max: %w", err)
	}
	if c.SessionTTL, err = time.ParseDuration(c.SessionSticky.TTL); err != nil {
		return fmt.Errorf("session_sticky.ttl: %w", err)
	}
	if c.SessionGCInterval, err = time.ParseDuration(c.SessionSticky.GCInterval); err != nil {
		return fmt.Errorf("session_sticky.gc_interval: %w", err)
	}
	// 指纹救场开关：直接携带（bool 无需解析）。
	c.SessionFingerprint = c.SessionSticky.FingerprintEnabled
	// 快过期窗口：空 = 禁用（ExpiringSoonDur 0）；非空必须可解析（拼写错误 fail fast）。
	if c.Pool.ExpiringSoon != "" {
		if c.ExpiringSoonDur, err = time.ParseDuration(c.Pool.ExpiringSoon); err != nil {
			return fmt.Errorf("pool.expiring_soon: %w", err)
		}
	}
	if c.ExpiringSoonDur < 0 {
		c.ExpiringSoonDur = 0
		c.Pool.ExpiringSoon = "0"
	}
	// costTier 探索窗口（issue #136）：空值回落默认 30m（Default 已置；此兜底覆盖
	// 显式 ""）；"0" 是合法值（关停，完全回到现状行为），不回落；负值钳 0 同关停
	//（"−5m" 无合理语义）。
	if c.Pool.CostExploreInterval == "" {
		c.Pool.CostExploreInterval = "30m"
	}
	if c.CostExploreIntervalDur, err = time.ParseDuration(c.Pool.CostExploreInterval); err != nil {
		return fmt.Errorf("pool.cost_explore_interval: %w", err)
	}
	if c.CostExploreIntervalDur < 0 {
		c.CostExploreIntervalDur = 0
	}
	// 积分保底：负值钳 0（= 关闭）。0 是合法默认（关闭），无需空值回落。
	if c.Pool.CreditFloor < 0 {
		c.Pool.CreditFloor = 0
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	// 连败降权参数缺省归一（非法/未设置回落默认，与 breaker_threshold 同风格）。
	if c.Pool.DegradeThreshold <= 0 {
		c.Pool.DegradeThreshold = 5
	}
	if c.Pool.DegradeCooldown == "" {
		c.Pool.DegradeCooldown = "10m"
	}
	if c.Pool.DegradeCooldownMax == "" {
		c.Pool.DegradeCooldownMax = "2h"
	}
	// global 在途分档：0/负数视为未设置回落默认 2（WAF 403 修复 P1-1）。
	if c.Pool.MaxInFlightGlobal <= 0 {
		c.Pool.MaxInFlightGlobal = 2
	}
	if c.Pool.IdleWeightPerHour <= 0 {
		c.Pool.IdleWeightPerHour = 0.5
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = 5.0
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	// header 缺省回落 timeout（保"首字节前换号"既有语义）；idle 缺省走内置大值。
	// 任务书约定：0 一律视为"未设置"走默认，真正的"禁用"留待后续（避免歧义）。
	if c.Upstream.HeaderTimeoutSeconds <= 0 {
		c.Upstream.HeaderTimeoutSeconds = c.Upstream.TimeoutSeconds
	}
	if c.Upstream.IdleTimeoutSeconds <= 0 {
		c.Upstream.IdleTimeoutSeconds = 300
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	// 空数组与 null 反序列化后覆盖掉 Default() 的排程值（键缺席才保留），在此补齐。
	// 空 = 未配置 → 回落默认；「禁用」一律走 *_enabled=false，两者互不混淆。
	if len(c.Schedule.CheckinHours) == 0 {
		c.Schedule.CheckinHours = []int{9, 21}
	}
	if len(c.Schedule.TravelHours) == 0 {
		c.Schedule.TravelHours = []int{9, 21}
	}
	if len(c.Schedule.ActivityHours) == 0 {
		c.Schedule.ActivityHours = []int{10}
	}
	if len(c.Schedule.KeepaliveHours) == 0 {
		c.Schedule.KeepaliveHours = []int{22}
	}
	if len(c.Schedule.BlackcatHours) == 0 {
		c.Schedule.BlackcatHours = []int{23}
	}
	if len(c.Schedule.GrowthHours) == 0 {
		c.Schedule.GrowthHours = []int{1}
	}
	// 余额后台刷新：启用时 minutes<=0 回落默认 5；关闭时 interval 保持 0（不启动）。
	if c.Schedule.BalanceRefreshEnabled {
		if c.Schedule.BalanceRefreshMinutes <= 0 {
			c.Schedule.BalanceRefreshMinutes = 5
		}
		c.BalanceRefreshInterval = time.Duration(c.Schedule.BalanceRefreshMinutes) * time.Minute
	}
	if err := c.validateScheduleHours(); err != nil {
		return err
	}
	// 裸名默认域：空/非法一律回落 cn（向后兼容，不 fail fast——旧 config 无此键）。
	c.ModelDefaultRealm = normalizeModelDefaultRealm(c.ModelDefaultRealm)
	// 出站代理：proxy_url / resin_url 为空 = 未接入；非空则解析并校验
	// （非法值 fail fast，避免面板保存后才在重启时暴露）。二者互斥。
	if c.ResinMode == "" {
		c.ResinMode = "reverse"
	}
	if c.ResinAuthVersion == "" {
		c.ResinAuthVersion = "V1"
	}
	pc, err := proxy.New(proxy.Config{
		ProxyURL:    c.ProxyURL,
		URL:         c.ResinURL,
		Platform:    c.ResinPlatformName,
		Mode:        c.ResinMode,
		AuthVersion: c.ResinAuthVersion,
	})
	if err != nil {
		return err
	}
	c.ProxyClient = pc
	if err := c.normalizeFingerprintRules(); err != nil {
		return err
	}
	// 工具结果图片策略：空 = auto（按入口默认）。这里只校验；生效由 main 的热应用
	// 路径统一 SetToolPolicy（校验失败时不能已经改掉进程级状态）。
	c.Media.ToolImages = strings.ToLower(strings.TrimSpace(c.Media.ToolImages))
	if err := media.ValidateToolPolicy(media.ToolPolicy(c.Media.ToolImages)); err != nil {
		return err
	}
	// 成长任务自动化策略（growth.autotasks）：归一化 + 非法值 fail fast。
	// 任务码是否存在于内置动作表由 panel 侧校验（配置域不依赖动作注册表，避免
	// 配置包反向依赖运行期组件）；未知码只告警不报错。
	if err := c.normalizeGrowthAutotasks(); err != nil {
		return err
	}
	// 图片像素处理策略（转码/压缩）：同样只校验，生效在 main 的热应用路径。
	if err := media.ValidateImagePolicy(media.ImagePolicy{
		Transcode:    c.Media.ImageTranscode,
		MaxDimension: c.Media.ImageMaxDimension,
	}); err != nil {
		return err
	}
	return c.normalizePrompt()
}

// normalizePrompt / buildPromptRule / normalizePromptMode 等见 prompt.go：
// 校验 prompt.mode（none/replace/append/after/inject，旧取值直接报错）、按域覆盖
// 构建 c.PromptRules（键 "" / cn / global），并在需要正文时解析 preset/file/text
// （素材优先级 text > file > preset，file 不可读 → fail fast）。

// validateScheduleHours 校验排程小时落在 0-23。
//
// 为什么不用 `[-1]` 之类的哨兵值表意"禁用"：非法小时被静默吞掉时，用户以为关掉了签到，
// 实际可能被当成另一个整点照常执行；这里直接快速失败，并在错误信息里指向正确的开关
// （checkin_enabled / keepalive_enabled），避免用户靠猜哨兵值来配。
func (c *Config) validateScheduleHours() error {
	if err := checkHourRange("schedule.checkin_hours", "checkin_enabled", c.Schedule.CheckinHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.travel_hours", "travel_enabled", c.Schedule.TravelHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.activity_hours", "activity_enabled", c.Schedule.ActivityHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.keepalive_hours", "keepalive_enabled", c.Schedule.KeepaliveHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.blackcat_hours", "blackcat_enabled", c.Schedule.BlackcatHours); err != nil {
		return err
	}
	return checkHourRange("schedule.growth_hours", "growth_enabled", c.Schedule.GrowthHours)
}

func checkHourRange(field, switchKey string, hours []int) error {
	for _, h := range hours {
		if h < 0 || h > 23 {
			return fmt.Errorf("%s: %d 不是合法小时（0-23）；如要关闭该任务请设 schedule.%s=false", field, h, switchKey)
		}
	}
	return nil
}

// normalizeGrowthAutotasks 归一化 growth.autotasks（成长任务自动化策略）。
//
// 只做三件事：去空白/去重/剔除空项、把可解析的取值解析成派生字段、对非法值
// fail fast（与 server.read_timeout / pool.breaker_cooldown 等同一风格：宁可
// 启动/保存时报错，也不静默回落成另一个行为）。
//
// 任务码的合法性不在这里校验：内置动作注册表在 internal/panel，配置包不反向
// 依赖运行期组件；panel 侧构建策略时对未知码打日志（面板运行日志可见）。
func (c *Config) normalizeGrowthAutotasks() error {
	g := &c.Growth.Autotasks
	g.Disabled = normalizeTaskCodes(g.Disabled)
	g.Only = normalizeTaskCodes(g.Only)
	g.Order = normalizeTaskCodes(g.Order)
	// mp_codes 保留 "+" 前缀语义，只 trim + 去重（"+x" 与 "x" 不算重复）。
	var mp []string
	seen := map[string]bool{}
	for _, raw := range g.MPCodes {
		code := strings.TrimSpace(raw)
		if code == "" || seen[code] {
			continue
		}
		if code == "+" {
			return fmt.Errorf("growth.autotasks.mp_codes: %q 只有前缀没有任务码", raw)
		}
		seen[code] = true
		mp = append(mp, code)
	}
	g.MPCodes = mp

	if len(g.Tasks) == 0 {
		g.Tasks = nil
		return nil
	}
	// 归一化任务码（trim）后再逐项解析。同名（trim 后）键视为配置错误：JSON 允许
	// " x " 与 "x" 并存，静默取其一会让配置含义随 map 遍历顺序漂移；先收集到新 map
	// 也避免边 range 边改原 map。
	trimmed := make(map[string]GrowthTaskPolicy, len(g.Tasks))
	for code, p := range g.Tasks {
		name := strings.TrimSpace(code)
		if name == "" {
			return fmt.Errorf("growth.autotasks.tasks: 任务码不能为空")
		}
		if _, dup := trimmed[name]; dup {
			return fmt.Errorf("growth.autotasks.tasks.%s: 重复的任务码（去掉首尾空白后同名）", name)
		}
		trimmed[name] = p
	}
	g.Tasks = trimmed
	for name, p := range g.Tasks {
		prefix := "growth.autotasks.tasks." + name
		p.ActionType = strings.TrimSpace(p.ActionType)
		switch p.ActionType {
		case "", "builtin", "chat_count", "model_chat", "mp_chat", "claim_only":
			// 合法模板
		default:
			return fmt.Errorf("%s.action_type: 未知动作模板 %q（可选：builtin, chat_count, model_chat, mp_chat, claim_only）", prefix, p.ActionType)
		}
		p.Model = strings.TrimSpace(p.Model)
		p.Gap = strings.TrimSpace(p.Gap)
		if p.Gap != "" {
			d, err := time.ParseDuration(p.Gap)
			if err != nil {
				return fmt.Errorf("%s.gap: %w", prefix, err)
			}
			if d < 0 {
				return fmt.Errorf("%s.gap: 负时长 %q 无意义", prefix, p.Gap)
			}
			p.GapDur = d
		}
		if p.Target < 0 {
			return fmt.Errorf("%s.target: 负值 %d 无意义（0 = 以上游下发为准）", prefix, p.Target)
		}
		p.ActivityID = strings.TrimSpace(p.ActivityID)
		p.Window = strings.TrimSpace(p.Window)
		if p.Window != "" {
			start, end, err := parseClockWindow(p.Window)
			if err != nil {
				return fmt.Errorf("%s.window: %w", prefix, err)
			}
			p.WindowStartMin, p.WindowEndMin, p.HasWindow = start, end, true
		}
		g.Tasks[name] = p
	}
	return nil
}

// normalizeTaskCodes trim + 去重 + 剔除空项（保持出现顺序）。
func normalizeTaskCodes(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, raw := range in {
		code := strings.TrimSpace(raw)
		if code == "" || seen[code] {
			continue
		}
		seen[code] = true
		out = append(out, code)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseClockWindow 解析 "HH:MM-HH:MM" 计数窗口，返回起止的当日分钟数
// （start > end 表示跨零点，如 23:00-08:00）。
func parseClockWindow(s string) (start, end int, err error) {
	sides := strings.Split(s, "-")
	if len(sides) != 2 {
		return 0, 0, fmt.Errorf("格式应为 HH:MM-HH:MM（如 23:00-08:00），得到 %q", s)
	}
	if start, err = parseClockMinute(sides[0]); err != nil {
		return 0, 0, err
	}
	if end, err = parseClockMinute(sides[1]); err != nil {
		return 0, 0, err
	}
	return start, end, nil
}

// parseClockMinute 解析 "HH:MM" 为当日分钟数（00:00-23:59）。
func parseClockMinute(s string) (int, error) {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("%q 不是 HH:MM 格式", s)
	}
	h, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || h < 0 || h > 23 {
		return 0, fmt.Errorf("%q 的小时不是 0-23", s)
	}
	m, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("%q 的分钟不是 0-59", s)
	}
	return h*60 + m, nil
}

// normalizeModelDefaultRealm 归一化 model_default_realm：小写去空白，接受
// cn/global/auto/auto:global,cn（及别名 cn,global / global,cn 等），
// 其余（含空）一律回落 cn（向后兼容：旧 config 无此键）。
func normalizeModelDefaultRealm(v string) string {
	d := strings.ToLower(strings.TrimSpace(v))
	d = strings.ReplaceAll(d, " ", "")
	switch d {
	case "global":
		return "global"
	case "auto", "auto:cn,global", "cn,global":
		return "auto"
	case "auto:global,cn", "global,cn":
		return "auto:global,cn"
	default:
		return "cn"
	}
}

// normalizeFingerprintRules 校验指纹配置（禁用项、覆盖项及自定义改写规则）并把归一化结果写回配置。
//
// 非法自定义规则在**启动/保存时报错**（scrub.Build 试编译，与历史行为一致）；
// 禁用/覆盖里的未知 ID 只告警并从配置剔除（不阻断启动，面板可见）。
func (c *Config) normalizeFingerprintRules() error {
	// 1. 归一化禁用项（去重、去空、未知 ID 告警）
	disSet := make(map[string]bool)
	if len(c.FingerprintDisabled) > 0 {
		cleaned := make([]string, 0, len(c.FingerprintDisabled))
		seen := make(map[string]bool)
		for _, raw := range c.FingerprintDisabled {
			id := strings.TrimSpace(raw)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			if !scrub.IsValidBuiltinID(id) {
				c.addWarning("fingerprint_disabled: 未知规则 ID " + id + "（已忽略）")
				continue
			}
			if id == "err-11128" {
				c.addWarning("fingerprint_disabled: err-11128 已被禁用——请求中出现裸 11128 将被上游整单拦截")
			}
			disSet[id] = true
			cleaned = append(cleaned, id)
		}
		if len(cleaned) == 0 {
			c.FingerprintDisabled = nil
		} else {
			c.FingerprintDisabled = cleaned
		}
	} else {
		c.FingerprintDisabled = nil
	}

	// 2. 归一化覆盖项（未知 ID 告警、长度校验）
	if len(c.FingerprintOverrides) > 0 {
		cleaned := make(map[string]string)
		for rawID, rawVal := range c.FingerprintOverrides {
			id := strings.TrimSpace(rawID)
			if id == "" {
				continue
			}
			if !scrub.IsValidBuiltinID(id) {
				c.addWarning("fingerprint_overrides: 未知规则 ID " + id + "（已忽略）")
				continue
			}
			if !scrub.ReplaceableBuiltin(id) {
				c.addWarning("fingerprint_overrides: " + id + " 是整段删除模式，替换文本不生效（已忽略；如需停用请用 fingerprint_disabled）")
				continue
			}
			val := strings.TrimSpace(rawVal)
			if val == "" {
				// 空串 = 用内置默认（与面板清空输入框的语义一致）。
				continue
			}
			if len(val) > 512 {
				return fmt.Errorf("fingerprint_overrides[%s]: 替换文本超过 512 字节", id)
			}
			if disSet[id] {
				c.addWarning("fingerprint_overrides: " + id + " 同时被 fingerprint_disabled 停用（覆盖当前不生效）")
			}
			cleaned[id] = val
		}
		if len(cleaned) == 0 {
			c.FingerprintOverrides = nil
		} else {
			c.FingerprintOverrides = cleaned
		}
	} else {
		c.FingerprintOverrides = nil
	}

	// 3. 校验自定义规则
	if len(c.FingerprintRules) > 0 {
		w, err := scrub.Build(c.FingerprintRules)
		if err != nil {
			return fmt.Errorf("fingerprint_rules: %w", err)
		}
		c.FingerprintRules = w.Rules()
	} else {
		c.FingerprintRules = nil
	}

	// 4. 自定义规则校验已完成（scrub.Build 试编译），层构建由 cmd/server 装配。
	return nil
}
