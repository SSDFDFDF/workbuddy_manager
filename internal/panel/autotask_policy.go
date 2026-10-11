// autotask_policy.go 成长任务自动化的**运行期策略层**：把 growth.autotasks 配置
// 与「内置动作注册表」（autotask.go 的 autoActions / builtinMPTaskCodes）合成一张可热替换
// 的快照，供任务中心、一键完成与 growth 排程共用。
//
// 分工（为什么是这样切）：
//   - 配置（internal/config）只描述「用户想让哪些任务自动化、用什么参数」，不认
//     判据事件形状；非法取值在启动/保存时 fail fast，任务码是否内置由这里判定
//     （配置包不反向依赖运行期组件）。
//   - 动作实现（autotask.go）仍是代码：判据事件的字段与埋点指纹是逆向产物，
//     配置只能引用已实现的动作，不能凭空造出判据。
//   - 策略快照（本文件）是两者之间唯一的桥：读侧只经 Panel.autotaskPolicy()，
//     保存配置时整体替换（原子指针），无锁无数据竞争。
//
// 缺省语义（策略为空）= 既有行为：内置动作全启用、内置顺序、内置 mp 码表、
// 内置参数（间隔 / 目标 / 窗口 / activityId）。因此老配置零影响。
package panel

import (
	"fmt"
	"log"
	"strings"
	"time"

	"workbuddy_manager/internal/auth"
	"workbuddy_manager/internal/config"
	"workbuddy_manager/internal/upstream"
)

// AutotaskPolicy 策略快照（构造后只读；整体替换）。
type AutotaskPolicy struct {
	// Disabled 屏蔽的任务码黑名单（与 Only 同时命中时 Disabled 优先）。
	Disabled map[string]bool
	// Only 白名单；HasOnly 为真时未列出的任务码不参与自动化。
	Only    map[string]bool
	HasOnly bool
	// Order 执行顺序覆盖：列出的码按本序在前，未列出的按内置依赖序排在其后。
	Order []string
	// MPCodes 配置显式登记的小程序口径任务码（已剥去 "+" 前缀）。
	// MPOverride 为真（配置里出现裸码）= 整体覆盖内置表，否则只做追加。
	MPCodes    map[string]bool
	MPOverride bool
	// AllowUnknownClaim 允许对「上游已下发但无判据动作实现」的任务只做
	// accept + 达标领奖（绝不伪造事件）。
	AllowUnknownClaim bool
	// Tasks 逐任务参数覆盖（键 = task_code）。
	Tasks map[string]TaskOption
}

// TaskOption 单个任务的参数覆盖（已解析；零值 = 用内置默认）。
type TaskOption struct {
	ActionType                   string
	Model                        string
	Gap                          time.Duration
	Target                       int64
	Attempt                      *bool
	HasWindow                    bool
	WindowStartMin, WindowEndMin int
	ActivityID                   string
}

// defaultAutotaskPolicy 空策略（= 既有行为）。Panel.autotask 为 nil 时读它，
// 使「从未 Set / 测试直接构造 Panel」的路径拿到与老代码逐字一致的行为。
var defaultAutotaskPolicy = &AutotaskPolicy{}

// NewAutotaskPolicy 由配置构建策略快照（cfg 为 nil = 空策略）。
//
// 未知任务码只打日志不报错：活动下线/码改名是常态，报错会让旧配置直接起不来；
// 而日志会镜像进面板「运行日志」，拼写错误不会被静默吞掉。
func NewAutotaskPolicy(cfg *config.Config) *AutotaskPolicy {
	pol := &AutotaskPolicy{}
	if cfg == nil {
		return pol
	}
	g := cfg.Growth.Autotasks
	if len(g.Disabled) > 0 {
		pol.Disabled = codeSet(g.Disabled)
	}
	if len(g.Only) > 0 {
		pol.Only = codeSet(g.Only)
		pol.HasOnly = true
	}
	pol.Order = append([]string(nil), g.Order...)
	if len(g.MPCodes) > 0 {
		pol.MPCodes = map[string]bool{}
		for _, raw := range g.MPCodes {
			code := strings.TrimSpace(raw)
			if rest, ok := strings.CutPrefix(code, "+"); ok {
				code = strings.TrimSpace(rest)
			} else {
				pol.MPOverride = true // 裸码 = 整体覆盖内置表
			}
			if code != "" {
				pol.MPCodes[code] = true
			}
		}
	}
	pol.AllowUnknownClaim = g.AllowUnknownClaim
	if len(g.Tasks) > 0 {
		pol.Tasks = make(map[string]TaskOption, len(g.Tasks))
		for code, p := range g.Tasks {
			pol.Tasks[code] = TaskOption{
				ActionType:     p.ActionType,
				Model:          p.Model,
				Gap:            p.GapDur,
				Target:         p.Target,
				Attempt:        p.Attempt,
				HasWindow:      p.HasWindow,
				WindowStartMin: p.WindowStartMin,
				WindowEndMin:   p.WindowEndMin,
				ActivityID:     p.ActivityID,
			}
		}
	}
	pol.warnUnknownCodes()
	return pol
}

// warnUnknownCodes 报告配置里引用的「非内置」任务码（拼写错误/活动已下线）。
// 只统计会实际参与动作决策的字段（Disabled/Only/Order/Tasks）。
func (pol *AutotaskPolicy) warnUnknownCodes() {
	if pol == nil {
		return
	}
	var unknown []string
	seen := map[string]bool{}
	note := func(code string) {
		if code == "" || seen[code] || autoActionByCode(code) != nil {
			return
		}
		// 配置了自定义动作模板的任务属明确声明，不视为未知码告警
		if opt, ok := pol.Tasks[code]; ok && opt.ActionType != "" && opt.ActionType != "builtin" {
			return
		}
		seen[code] = true
		unknown = append(unknown, code)
	}
	for code := range pol.Disabled {
		note(code)
	}
	for code := range pol.Only {
		note(code)
	}
	for _, code := range pol.Order {
		note(code)
	}
	for code := range pol.Tasks {
		note(code)
	}
	if len(unknown) == 0 {
		return
	}
	log.Printf("panel: growth.autotasks 引用了 %d 个未内置的任务码（拼写错误或活动已下线，相关策略对其无效）：%s",
		len(unknown), strings.Join(unknown, ", "))
}

// codeSet 把已归一化的任务码列表转成集合。
func codeSet(codes []string) map[string]bool {
	out := make(map[string]bool, len(codes))
	for _, c := range codes {
		if c != "" {
			out[c] = true
		}
	}
	return out
}

// SetAutotaskPolicy 整体替换策略快照（配置保存的热应用入口，nil = 回到默认）。
func (p *Panel) SetAutotaskPolicy(pol *AutotaskPolicy) {
	if pol == nil {
		pol = &AutotaskPolicy{}
	}
	p.autotask.Store(pol)
	// mp 码口径可能随配置改变（覆盖/追加），探测缓存保留无妨：它只做并集。
}

// autotaskPolicy 返回当前策略快照（nil-safe：从未设置或测试构造时为默认空策略）。
func (p *Panel) autotaskPolicy() *AutotaskPolicy {
	if pol := p.autotask.Load(); pol != nil {
		return pol
	}
	return defaultAutotaskPolicy
}

// ---------------------------------------------------------------------------
// 启用集合与顺序
// ---------------------------------------------------------------------------

// actionFor 查任务当前生效的动作：内置表 ∩ 策略（黑/白名单），未内置但允许
// 兜底时合成「仅接受 + 达标领奖」动作；否则返回 nil（该任务不参与自动化）。
//
// 返回的是**副本**：Attempt 等展示标记可被逐任务覆盖而不污染全局注册表。
func (p *Panel) actionFor(code string) *autoAction {
	pol := p.autotaskPolicy()
	if pol.Disabled[code] {
		return nil
	}
	if pol.HasOnly && !pol.Only[code] {
		return nil
	}
	opt := pol.Tasks[code]
	// 1. 若配置了自定义动作模板（chat_count / model_chat / mp_chat / claim_only），优先派生对应动作
	if opt.ActionType != "" && opt.ActionType != "builtin" {
		if act := p.actionFromTemplate(code, opt); act != nil {
			return act
		}
	}
	// 2. 内置硬编码动作表（最高优先级预置保障）：
	if act := autoActionByCode(code); act != nil {
		out := *act
		if opt.Attempt != nil {
			out.Attempt = *opt.Attempt
		}
		return &out
	}
	// 3. 兜底动作（未内置且未配模板，策略允许时做 accept + 达标领奖）：
	if pol.AllowUnknownClaim && plausibleTaskCode(code) {
		return p.claimOnlyAction(code)
	}
	return nil
}

// actionFromTemplate 根据配置的动作模板动态生成动作执行器。
func (p *Panel) actionFromTemplate(code string, opt TaskOption) *autoAction {
	attempt := opt.Attempt != nil && *opt.Attempt
	switch opt.ActionType {
	case "chat_count":
		return &autoAction{
			TaskCode: code,
			Desc:     "通用对话活跃上报模板（自动补足差额）",
			Attempt:  attempt,
			run: func(p *Panel, a *auth.Auth) (string, error) {
				return p.runDynamicChatCount(a, code)
			},
		}
	case "model_chat":
		model := opt.Model
		if model == "" {
			model = "glm-5.2"
		}
		return &autoAction{
			TaskCode: code,
			Desc:     fmt.Sprintf("通用模型对话打卡模板（模型：%s）", model),
			Attempt:  attempt,
			run: func(p *Panel, a *auth.Auth) (string, error) {
				return p.runDynamicModelChat(a, code, model)
			},
		}
	case "mp_chat":
		return &autoAction{
			TaskCode: code,
			Desc:     "通用小程序对话上报模板",
			Attempt:  attempt,
			run: func(p *Panel, a *auth.Auth) (string, error) {
				return p.runDynamicMPChat(a, code)
			},
		}
	case "claim_only":
		return p.claimOnlyAction(code)
	default:
		return nil
	}
}

// actions 返回策略过滤后的内置动作表（按策略顺序，供「一键完成」遍历）。
func (p *Panel) actions() []autoAction {
	out := make([]autoAction, 0, len(autoActions))
	for _, code := range p.orderedCodes() {
		if act := p.actionFor(code); act != nil {
			out = append(out, *act)
		}
	}
	return out
}

// orderedCodes 内置动作码按策略顺序排列（Order 列表内按序在前，未列出的按内置依赖序）。
func (p *Panel) orderedCodes() []string {
	pol := p.autotaskPolicy()
	out := make([]string, 0, len(autoActions))
	placed := map[string]bool{}
	for _, code := range pol.Order {
		if autoActionByCode(code) == nil || placed[code] {
			continue // 未内置/重复项不参与排序（未知码已由 warnUnknownCodes 提示）
		}
		placed[code] = true
		out = append(out, code)
	}
	for _, act := range autoActions {
		if !placed[act.TaskCode] {
			placed[act.TaskCode] = true
			out = append(out, act.TaskCode)
		}
	}
	// 配置了自定义模板动作的任务也排入顺序列表
	for code, opt := range pol.Tasks {
		if opt.ActionType != "" && opt.ActionType != "builtin" && !placed[code] {
			placed[code] = true
			out = append(out, code)
		}
	}
	return out
}

// actionIndex 排序键：策略 Order 中的位置在前（0..n-1），未列出的按
// n+内置序 排在后面。未知码给大值（排最后）。
func (p *Panel) actionIndex(code string) int {
	pol := p.autotaskPolicy()
	for i, c := range pol.Order {
		if c == code {
			return i
		}
	}
	base := autoActionIndex(code)
	if base >= 1<<20 {
		return 1 << 20 // 未知码：等大值，保持原本的稳定排序语义
	}
	return len(pol.Order) + base
}

// ---------------------------------------------------------------------------
// 逐任务参数覆盖
// ---------------------------------------------------------------------------

// taskOption 返回任务参数覆盖（无覆盖 = 零值）。
func (p *Panel) taskOption(code string) TaskOption {
	return p.autotaskPolicy().Tasks[code]
}

// gapFor 返回任务的事件间隔：配置覆盖（>0）优先，否则 def。
func (p *Panel) gapFor(code string, def time.Duration) time.Duration {
	if d := p.taskOption(code).Gap; d > 0 {
		return d
	}
	return def
}

// targetFor 返回任务的进度目标：配置覆盖（>0）优先，否则 def。
func (p *Panel) targetFor(code string, def int64) int64 {
	if n := p.taskOption(code).Target; n > 0 {
		return n
	}
	return def
}

// activityIDFor 返回事件 activityId 覆盖（空 = 用内置默认）。
func (p *Panel) activityIDFor(code string) string {
	return p.taskOption(code).ActivityID
}

// inCountWindow 报告 now 是否落在任务的计数窗口内（配置可覆盖；支持跨零点）。
// 判定实现与排程器共用 upstream 的同一套：覆盖用 InWindow，缺省用 InNightWindow
// （内置 23:00–08:00），避免「面板说在窗口内、排程说不在」的分裂。
func (p *Panel) inCountWindow(code string, now time.Time) bool {
	if ov := p.taskOption(code); ov.HasWindow {
		return upstream.InWindow(ov.WindowStartMin, ov.WindowEndMin, now)
	}
	return upstream.InNightWindow(now)
}

// ---------------------------------------------------------------------------
// 小程序口径（mp）任务码
// ---------------------------------------------------------------------------

// mpDetectTTL 运行时 mp 码探测结果的缓存时长（探测本身来自每次任务列表拉取，
// 缓存只为「只有单侧列表可用」的路径兜底）。
var mpDetectTTL = 30 * time.Minute

// isMPTaskCode 报告任务是否小程序口径专属：内置表 ∪ 配置 ∪ 运行时探测。
// 决定回读 / 接受 / 领奖是否带 X-Client-Platform: miniprogram。
func (p *Panel) isMPTaskCode(code string) bool {
	pol := p.autotaskPolicy()
	if !pol.MPOverride && builtinMPTaskCodes[code] {
		return true
	}
	if pol.MPCodes[code] {
		return true
	}
	p.mpMu.Lock()
	defer p.mpMu.Unlock()
	if time.Since(p.mpDetectedAt) > mpDetectTTL {
		return false
	}
	return p.mpDetected[code]
}

// noteMPDetected 记录「默认列表有 / mp 列表有」两侧差集得到的新 mp 专属码。
// 只在两次列表都成功时调用（单侧失败会把全部任务误判成 mp 专属）。
// 即使差集为空也算一次成功探测（刷新时间戳，避免已记住的码被 TTL 误清）。
func (p *Panel) noteMPDetected(codes []string) {
	p.mpMu.Lock()
	defer p.mpMu.Unlock()
	if len(codes) > 0 {
		if p.mpDetected == nil {
			p.mpDetected = map[string]bool{}
		}
		for _, c := range codes {
			if c != "" {
				p.mpDetected[c] = true
			}
		}
	}
	p.mpDetectedAt = time.Now()
}

// mpExclusiveCodes 返回 mp 列表相对默认列表的差集（mp 专属码）。
//
// 默认列表为空时返回 nil（不记录）：空列表可能是上游瞬时异常的空响应，
// 此时"差集 = 全部 mp 任务"会把常规任务（chat_5 等）误判成 mp 专属；
// 已知 mp 码本就有内置表兜底，代价远小于误判。
func mpExclusiveCodes(defaultCodes, mpCodes []upstream.Task) []string {
	if len(defaultCodes) == 0 {
		return nil
	}
	inDefault := make(map[string]bool, len(defaultCodes))
	for _, t := range defaultCodes {
		inDefault[t.TaskCode] = true
	}
	var out []string
	for _, t := range mpCodes {
		if t.TaskCode != "" && !inDefault[t.TaskCode] {
			out = append(out, t.TaskCode)
		}
	}
	return out
}

// plausibleTaskCode 报告 code 是否像一个真实任务码（用于 allow_unknown_claim
// 兜底）：排除空串与内部伪码（如 "(批量接受)"）。
func plausibleTaskCode(code string) bool {
	return code != "" && !strings.HasPrefix(code, "(")
}

// ---------------------------------------------------------------------------
// 未知任务的兜底动作：accept + 达标领奖（绝不伪造事件）
// ---------------------------------------------------------------------------

// claimOnlyAction 合成「仅接受 + 达标领奖」动作（策略 AllowUnknownClaim 开启时
// 对未内置判据的任务生效）。claimOnly 标记决定它只在该任务已达标时进入待办，
// 避免每天把「永远等不到判据」的任务排进队列。
func (p *Panel) claimOnlyAction(code string) *autoAction {
	return &autoAction{
		TaskCode:  code,
		Desc:      "未内置判据事件：仅接受任务 + 达标领奖（不伪造事件）",
		claimOnly: true,
		run: func(p *Panel, a *auth.Auth) (string, error) {
			return p.runClaimOnly(a, code)
		},
	}
}

// runClaimOnly 未内置判据任务的唯一动作：必要时 accept；已达标则领奖；
// 未达标原样返回进度（不产生任何事件上报）。
func (p *Panel) runClaimOnly(a *auth.Auth, code string) (string, error) {
	mp := p.isMPTaskCode(code)
	lookup := func() (*upstream.Task, error) {
		if mp {
			return p.taskByCodeMP(a, code)
		}
		return p.taskByCode(a, code)
	}
	t, err := lookup()
	if err != nil {
		return "", err
	}
	if t == nil {
		return "该账号无此任务（可能已下线/未下发）", nil
	}
	if t.Claimed {
		return "已领取", nil
	}
	needAccept := t.AcceptStatus == "not_accepted" || t.AcceptStatus == ""
	reached := t.Claimable || t.AcceptStatus == "completed" || (t.Target > 0 && t.Current >= t.Target)
	if !reached {
		if needAccept {
			if err := p.acceptTask(a, code, mp); err != nil {
				return fmt.Sprintf("接受任务失败: %v", err), nil
			}
		}
		return fmt.Sprintf("进度未达标（%s）：已保持已接受状态，达标后自动领奖", taskProgressText(t)), nil
	}
	if needAccept {
		if err := p.acceptTask(a, code, mp); err != nil {
			return fmt.Sprintf("接受任务失败: %v", err), nil
		}
	}
	credit, energy, err := p.claimTask(a, code, mp)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("任务已达标并领取奖励（+%dc +%de）", credit, energy), nil
}

// acceptTask 接受任务（mp 口径带登记回读验证；默认口径直接提交）。
func (p *Panel) acceptTask(a *auth.Auth, code string, mp bool) error {
	if mp {
		if !p.acceptWithVerifyMP(a, code) {
			return fmt.Errorf("accept 未登记生效（上游 200+OK 但未落账），待下次重试")
		}
		return nil
	}
	return p.cfg.Upstream.AcceptTasks(a, []string{code})
}

// claimTask 领取任务奖励（按口径选端点，返回新增 credit/energy）。
func (p *Panel) claimTask(a *auth.Auth, code string, mp bool) (int64, int64, error) {
	if mp {
		return p.cfg.Upstream.ClaimRewardMP(a, code)
	}
	return p.cfg.Upstream.ClaimReward(a, code)
}
