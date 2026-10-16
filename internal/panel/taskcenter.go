// taskcenter.go 面板「任务中心」：全账号任务扫描 + 执行队列（可配并发）+
// 成长任务队列。
//
// 语义：
//   - 扫描（scan_all）：并发拉取每账号的成长任务列表（默认+小程序口径），
//     汇总出"未完成且可自动化"的待办清单（只读，不执行）。
//   - 执行队列（run_queue + queue）：把待办项按账号分组排队执行——账号内
//     串行（复用 per-account 锁，与单任务/一键完成互斥），账号间并发
//     （concurrency 信号量限制，默认 1）。队列状态可轮询。
package panel

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"workbuddy_manager/internal/auth"
	"workbuddy_manager/internal/config"
	"workbuddy_manager/internal/upstream"
)

// ---------------------------------------------------------------------------
// 扫描（只读）
// ---------------------------------------------------------------------------

// scanAccountItem 单账号扫描结果。
type scanAccountItem struct {
	UID       string          `json:"uid"`
	Nickname  string          `json:"nickname"`
	Growth    []upstream.Task `json:"growth,omitempty"`
	GrowthErr string          `json:"growth_error,omitempty"`
}

// growthPending 报告任务是否「未完成且可自动化」——判定完全由当前策略
// （growth.autotasks）驱动：被黑/白名单屏蔽的任务不出现，allow_unknown_claim
// 合成的兜底动作只在任务已达标时入待办（否则每天白跑一轮 accept）。
func (p *Panel) growthPending(t upstream.Task) bool {
	if t.Claimed {
		return false
	}
	// 上游锁定的任务不出待办：Sequential 族每日零点解锁一环，刚做完上一环时
	// 下一环以下发但 locked 形态出现在列表里——扫进队列只会 accept 不落账报
	// 失败（每日锁定窗口），零点解锁后自然回到待办。其余 locked（上游未开放）
	// 同语义：不该被自动化尝试。
	if t.Locked {
		return false
	}
	act := p.actionFor(t.TaskCode)
	if act == nil {
		return false
	}
	// 兜底动作（未内置判据）只做 accept + 领奖：没达标时排队毫无意义。
	if act.claimOnly {
		return t.Claimable || t.AcceptStatus == "completed" || (t.Target > 0 && t.Current >= t.Target)
	}
	return true
}

// pendingGrowthTasks 拉取该账号「未完成且可自动化」的成长任务待办
// （默认口径 ∪ 小程序口径，按 task_code 去重）。
//
// 为什么要收成一个函数：任务中心的两个入口（只读扫描 tasksScanAll 与执行队列
// startGrowthQueue）此前各写一份「拉列表 + 过滤 + 合并 mp」逻辑，已经在 mp 列表
// 失败时的行为上分叉——扫描显示 mp 待办，而队列报"无可执行待办"（实修过一次）。
// 待办口径必须只有一份实现，否则下次改动会再次分叉。
//
// realm/企业版门控由调用方先行判定（调用方还有各自的日志/字段语义）。
// mp 列表失败静默（无 mp 任务的部署/活动结束时零影响）；返回 error 仅指默认口径失败。
func (p *Panel) pendingGrowthTasks(a *auth.Auth) ([]upstream.Task, error) {
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		return nil, err
	}
	var pending []upstream.Task
	seen := map[string]bool{}
	for _, t := range tasks {
		if p.growthPending(t) {
			pending = append(pending, t)
			seen[t.TaskCode] = true
		}
	}
	// 小程序口径任务（school_season 校园日 / Sequential_Tasks_1 小程序首对话）
	// 仅在 mp 头列表下发，与默认口径不重叠——合并进待办；失败静默。
	// 两次列表都成功时顺手记录差集（= mp 专属码），供 isMPTaskCode 运行时识别。
	if mpTasks, mpErr := p.cfg.Upstream.ListTasksMP(a); mpErr == nil {
		p.noteMPDetected(mpExclusiveCodes(tasks, mpTasks))
		p.recordKnownTasks(mpTasks)
		for _, t := range mpTasks {
			if p.growthPending(t) && !seen[t.TaskCode] {
				pending = append(pending, t)
				seen[t.TaskCode] = true
			}
		}
	}
	p.recordKnownTasks(tasks)
	return pending, nil
}

// tasksScanAll 扫描全部账号：成长任务（未完成+可自动化，含 mp 口径合并）。
// 只读操作，并发拉取（账号数个位数）。
func (p *Panel) tasksScanAll(w http.ResponseWriter, r *http.Request) {
	states := p.cfg.Pool.List()
	items := make([]scanAccountItem, len(states))
	var wg sync.WaitGroup
	for i, st := range states {
		if st.Disabled {
			continue
		}
		wg.Add(1)
		go func(i int, uid string) {
			defer wg.Done()
			a := p.cfg.Pool.AuthByUID(uid)
			if a == nil {
				return
			}
			it := &items[i]
			it.UID, it.Nickname = uid, a.NicknameValue()
			// D4 门控：global 账号无 CN 成长任务体系，不发起任何上游调用。
			if a.IsGlobal() {
				return
			}
			// 企业版门控：同上——企业版无个人成长体系（GET /v2/activity/growth/tasks
			// 上游 403「growth system is only available for personal users」）。
			if a.IsEnterprise() {
				return
			}
			pending, err := p.pendingGrowthTasks(a)
			if err != nil {
				it.GrowthErr = err.Error()
				return
			}
			it.Growth = pending
		}(i, st.UID)
	}
	wg.Wait()
	pending := 0
	for _, it := range items {
		pending += len(it.Growth)
	}
	log.Printf("panel: 队列扫描完成：全部账号待办 %d 项", pending)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": items, "pending_count": pending})
}

// ---------------------------------------------------------------------------
// 执行队列
// ---------------------------------------------------------------------------

// queueItem 队列执行单元。
type queueItem struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Kind     string `json:"kind"` // growth
	Code     string `json:"code"`
	Status   string `json:"status"` // pending | running | done | skipped | error
	Message  string `json:"message,omitempty"`
}

// queueState 队列运行状态。Seq 每次启动 +1——前端只渲染"自己启动的那一轮"，
// 执行结束后的残留 items 不会覆盖后续的扫描结果视图。
type queueState struct {
	mu        sync.Mutex
	running   bool
	startedAt time.Time
	items     []queueItem
	conc      int
	seq       int
}

// Panel 队列字段在 Panel 结构体上（panel.go）由 initQueue 惰性初始化；
// 这里集中访问器，避免改动 New 构造链。
func (p *Panel) queue() *queueState {
	p.queueOnce.Do(func() { p.q = &queueState{} })
	return p.q
}

// tasksRunQueue 启动执行队列：{concurrency:1-4, growth:bool, school:bool}。
// 先做一次扫描，把全部待办项排队（growth 按账号内 autoActions 顺序执行，
// school 逐账号跑闭环），账号内串行、账号间受并发信号量约束。
func (p *Panel) tasksRunQueue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Concurrency int  `json:"concurrency"`
		Growth      bool `json:"growth"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if !body.Growth {
		body.Growth = true
	}
	if body.Concurrency < 1 {
		body.Concurrency = 1
	}
	if body.Concurrency > 4 {
		body.Concurrency = 4
	}
	started, total, seq, msg := p.startGrowthQueue(body.Concurrency, body.Growth)
	switch {
	case seq == -1:
		writeErr(w, http.StatusConflict, msg)
	case !started:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": false, "message": msg})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true, "total": total, "seq": seq})
	}
}

// startGrowthQueue 扫描全部账号待办并启动队列（HTTP「执行全部待办」与调度器
// growth 时点共用核心）。返回 (started, total, seq, msg)：seq==-1 表示队列
// 已在执行（冲突）；started=false 时 msg 为无可执行待办的说明。并发夹取
// [1,4]；growth 开关同 HTTP 入参语义。
func (p *Panel) startGrowthQueue(concurrency int, growth bool) (started bool, total int, seq int, msg string) {
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 4 {
		concurrency = 4
	}
	q := p.queue()
	q.mu.Lock()
	if q.running {
		q.mu.Unlock()
		return false, 0, -1, "队列正在执行中（可在任务中心查看进度）"
	}
	// 先占位：扫描（数秒级网络耗时）期间若并发再次触发，直接命中上面的 running
	// 判拒，避免两个 goroutine 同时启动互相覆盖 q.items/q.seq。无待办时回滚。
	q.running = true
	q.startedAt = time.Now()
	q.mu.Unlock()

	// 扫描待办（复用扫描逻辑的拉取部分）。
	states := p.cfg.Pool.List()

	var accts []queueAccount
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, st := range states {
		if st.Disabled {
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(a *auth.Auth) {
			defer wg.Done()
			one := queueAccount{a: a}
			// D4 门控：global 账号无 CN 成长任务体系，不发起任何上游调用。
			if a.IsGlobal() {
				return
			}
			// 企业版门控：同上（企业版无个人成长体系，任务端点上游一律 403）。
			if a.IsEnterprise() {
				return
			}
			if growth {
				if pending, err := p.pendingGrowthTasks(a); err == nil {
					one.grow = pending
					sort.Slice(one.grow, func(i, j int) bool { // 按策略顺序（缺省 = 内置依赖序）
						return p.actionIndex(one.grow[i].TaskCode) < p.actionIndex(one.grow[j].TaskCode)
					})
				}
			}
			if len(one.grow) > 0 {
				mu.Lock()
				accts = append(accts, one)
				mu.Unlock()
			}
		}(a)
	}
	wg.Wait()

	// 组装队列（账号分组，保持顺序）。
	var items []queueItem
	for _, one := range accts {
		for _, t := range one.grow {
			items = append(items, queueItem{UID: one.a.UID, Nickname: one.a.NicknameValue(), Kind: "growth", Code: t.TaskCode, Status: "pending"})
		}
	}
	if len(items) == 0 {
		log.Printf("panel: 队列启动：无可执行待办（全部账号任务已完成）")
		q.mu.Lock()
		q.running = false
		q.startedAt = time.Time{}
		q.mu.Unlock()
		return false, 0, 0, "全部账号没有待办任务"
	}

	q.mu.Lock()
	q.items = items
	q.conc = concurrency
	q.seq++
	seq = q.seq
	q.mu.Unlock()

	go p.runQueueItems(accts, items, concurrency)
	log.Printf("panel: 队列启动：%d 项（并发 %d，成长 %v）", len(items), concurrency, growth)
	return true, len(items), seq, ""
}

// RunGrowthQueueOnce 调度器 growth 时点回调（sch.SetGrowthHook 挂载）：与
// 「执行全部待办」按钮完全同管线（成长，串行并发 1）。Sequential 族
// 每日零点解锁一环，此前只能手动扫描推进；此回调让链条每天自动走一环。
// 异步执行（startGrowthQueue 启动 goroutine 即返），已在跑/无待办安全跳过。
func (p *Panel) RunGrowthQueueOnce() {
	started, total, _, _ := p.startGrowthQueue(1, true)
	if started {
		log.Printf("panel: 定时成长任务队列已启动（%d 项）", total)
	}
}

// runQueueItems 队列执行主体：按账号分组，账号内串行（per-account 锁），
// 账号间并发（信号量）。每项结果写回队列状态。
func (p *Panel) runQueueItems(accts []queueAccount, items []queueItem, concurrency int) {
	q := p.queue()
	defer func() {
		q.mu.Lock()
		q.running = false
		q.mu.Unlock()
		log.Printf("panel: 队列执行结束（共 %d 项）", len(items))
	}()

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, one := range accts {
		wg.Add(1)
		go func(one queueAccount) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// per-account 互斥：与单任务/一键完成共用一把锁。
			if !p.tryLockAccount(one.a.UID) {
				p.queueSet(q, one.a.UID, func(it *queueItem) {
					it.Status, it.Message = "skipped", "该账号有其它任务动作在执行，跳过"
				})
				return
			}
			defer p.unlockAccount(one.a.UID)
			// 前置：批量接受尚未接受的任务。上游对 not_accepted 的任务不计数——
			// 面板「一键完成」一直有这步，队列路径此前漏了（表现为上报 200 但进度
			// 一直 not_accepted、无法领奖）。失败不阻塞（行为事件才是进度判据）。
			if accepted := p.acceptPendingTasks(one.a); accepted > 0 {
				time.Sleep(reportGap) // 给上游状态流转留时间
			}
			for i := range q.items {
				uid, kind, code := q.snapshotAt(i)
				if uid != one.a.UID {
					continue
				}
				p.queueMarkAt(i, "running", "")
				var msg string
				var err error
				switch kind {
				case "growth":
					msg, err = p.runGrowthQueued(one.a, code)
				}
				if err != nil {
					p.queueMarkAt(i, "error", err.Error())
				} else {
					p.queueMarkAt(i, "done", msg)
				}
				time.Sleep(reportGap) // 项间节流
			}
		}(one)
	}
	wg.Wait()
}

// queueAccount 队列执行的账号单元（runQueueItems 参数）。
type queueAccount struct {
	a    *auth.Auth
	grow []upstream.Task
}

// snapshotAt 锁内读条目三元组（避免锁外持有指针）。
func (q *queueState) snapshotAt(i int) (uid, kind, code string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.items[i].UID, q.items[i].Kind, q.items[i].Code
}

// queueMarkAt 按索引更新队列条目状态（条目数组固定不再增删）。
func (p *Panel) queueMarkAt(i int, status, msg string) {
	q := p.queue()
	q.mu.Lock()
	q.items[i].Status, q.items[i].Message = status, msg
	q.mu.Unlock()
}

// queueSet 按 uid 批量改状态。
func (p *Panel) queueSet(q *queueState, uid string, fn func(*queueItem)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].UID == uid {
			fn(&q.items[i])
		}
	}
}

// acceptPendingTasks 批量接受该账号未接受的任务，返回接受的个数（失败返回 0 不阻塞）。
func (p *Panel) acceptPendingTasks(a *auth.Auth) int {
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		return 0
	}
	var codes []string
	for _, t := range tasks {
		if !t.Claimed && !t.Locked && t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
			codes = append(codes, t.TaskCode)
		}
	}
	if len(codes) == 0 {
		return 0
	}
	if err := p.cfg.Upstream.AcceptTasks(a, codes); err != nil {
		log.Printf("panel: 队列 accept uid=%s: %v（不阻塞）", a.UID, err)
		return 0
	}
	log.Printf("panel: 队列 accept uid=%s: 已接受 %d 个任务", a.UID, len(codes))
	return len(codes)
}

// runGrowthQueued 执行单个成长任务（动作 + 回读 + 自动领奖；与
// accountTaskAuto 同语义，结果以文字返回）。
func (p *Panel) runGrowthQueued(a *auth.Auth, code string) (string, error) {
	act := p.actionFor(code)
	if act == nil {
		return "", fmt.Errorf("任务 %s 无自动动作（可能被 growth.autotasks 配置屏蔽）", code)
	}
	// taskByCode 已双口径（mp 专属码自动回落 mp 列表）。
	before, err := p.taskByCode(a, code)
	if err != nil {
		return "", err
	}
	if before == nil {
		return "该账号无此任务", nil
	}
	isMP := p.isMPTaskCode(code)
	if before.Claimed {
		return "已完成（已领取）", nil
	}
	msg, err := act.run(p, a)
	if err != nil {
		return "", err
	}
	var after *upstream.Task
	if isMP {
		after, _ = p.taskByCodeMP(a, code)
	} else {
		after, _ = p.taskByCodeWaiting(a, code)
	}
	if after != nil && after.Claimable {
		var credit, energy int64
		var cerr error
		if isMP {
			credit, energy, cerr = p.cfg.Upstream.ClaimRewardMP(a, code)
		} else {
			credit, energy, cerr = p.cfg.Upstream.ClaimReward(a, code)
		}
		if cerr == nil && (credit > 0 || energy > 0) {
			msg += fmt.Sprintf("；自动领奖 +%d 分 +%d 能", credit, energy)
		}
	}
	if after != nil {
		msg += "（进度 " + taskProgressText(after) + "）"
	}
	log.Printf("panel: 队列 growth uid=%s code=%s: %s", a.UID, code, msg)
	return msg, nil
}

// tasksActions 列出内置自动化动作与当前策略下的启用状态（供面板「任务中心 / 配置
// 页」展示可配置的任务码；代码是注册表的唯一真相，前端不硬编码清单）。
type taskActionInfo struct {
	Code         string `json:"code"`
	Title        string `json:"title,omitempty"`
	Desc         string `json:"desc,omitempty"`
	Attempt      bool   `json:"attempt,omitempty"`
	Enabled      bool   `json:"enabled"`
	MP           bool   `json:"mp,omitempty"`
	Builtin      bool   `json:"builtin"`
	ActionType   string `json:"action_type,omitempty"`
	Model        string `json:"model,omitempty"`
	Gap          string `json:"gap,omitempty"`
	Target       int64  `json:"target,omitempty"`
	Window       string `json:"window,omitempty"`
	ActivityID   string `json:"activity_id,omitempty"`
	Unconfigured bool   `json:"unconfigured,omitempty"`
}

func (p *Panel) tasksActions(w http.ResponseWriter, r *http.Request) {
	pol := p.autotaskPolicy()
	out := make([]taskActionInfo, 0, len(autoActions))
	seen := map[string]bool{}

	// 1. 内置硬编码动作表（按排序列表输出）
	for _, code := range p.orderedCodes() {
		base := autoActionByCode(code)
		if base == nil {
			continue
		}
		seen[code] = true
		act := p.actionFor(code)
		opt := pol.Tasks[code]
		title := ""
		if kt, ok := p.getKnownTask(code); ok && kt.Title != "" {
			title = kt.Title
		}
		actionType := opt.ActionType
		if actionType == "" {
			actionType = "builtin"
		}
		gapStr := ""
		if opt.Gap > 0 {
			gapStr = opt.Gap.String()
		}
		windowStr := ""
		if opt.HasWindow {
			windowStr = fmt.Sprintf("%02d:%02d-%02d:%02d", opt.WindowStartMin/60, opt.WindowStartMin%60, opt.WindowEndMin/60, opt.WindowEndMin%60)
		}
		out = append(out, taskActionInfo{
			Code:       code,
			Title:      title,
			Desc:       base.Desc,
			Attempt:    act != nil && act.Attempt,
			Enabled:    act != nil,
			MP:         p.isMPTaskCode(code),
			Builtin:    true,
			ActionType: actionType,
			Model:      opt.Model,
			Gap:        gapStr,
			Target:     opt.Target,
			Window:     windowStr,
			ActivityID: opt.ActivityID,
		})
	}

	// 2. 配置了自定义动作模板的任务（不在 autoActions 中）
	for code, opt := range pol.Tasks {
		if seen[code] {
			continue
		}
		seen[code] = true
		act := p.actionFor(code)
		title := ""
		desc := ""
		if kt, ok := p.getKnownTask(code); ok {
			title = kt.Title
			desc = kt.TaskDesc
			if desc == "" {
				desc = kt.Description
			}
		}
		if desc == "" && act != nil {
			desc = act.Desc
		}
		gapStr := ""
		if opt.Gap > 0 {
			gapStr = opt.Gap.String()
		}
		windowStr := ""
		if opt.HasWindow {
			windowStr = fmt.Sprintf("%02d:%02d-%02d:%02d", opt.WindowStartMin/60, opt.WindowStartMin%60, opt.WindowEndMin/60, opt.WindowEndMin%60)
		}
		out = append(out, taskActionInfo{
			Code:       code,
			Title:      title,
			Desc:       desc,
			Attempt:    act != nil && act.Attempt,
			Enabled:    act != nil,
			MP:         p.isMPTaskCode(code),
			Builtin:    false,
			ActionType: opt.ActionType,
			Model:      opt.Model,
			Gap:        gapStr,
			Target:     opt.Target,
			Window:     windowStr,
			ActivityID: opt.ActivityID,
		})
	}

	// 3. 运行时探测/扫描发现但尚未配置动作的任务
	p.knownTasksMu.RLock()
	var unconfigured []upstream.Task
	for code, t := range p.knownTasks {
		if !seen[code] {
			unconfigured = append(unconfigured, t)
		}
	}
	p.knownTasksMu.RUnlock()
	sort.Slice(unconfigured, func(i, j int) bool {
		return unconfigured[i].TaskCode < unconfigured[j].TaskCode
	})
	for _, t := range unconfigured {
		desc := t.TaskDesc
		if desc == "" {
			desc = t.Description
		}
		out = append(out, taskActionInfo{
			Code:         t.TaskCode,
			Title:        t.Title,
			Desc:         desc,
			Enabled:      false,
			MP:           p.isMPTaskCode(t.TaskCode),
			Builtin:      false,
			ActionType:   "",
			Unconfigured: true,
		})
	}

	body := map[string]any{
		"ok":      true,
		"actions": out,
		"policy": map[string]any{
			"has_only":            pol.HasOnly,
			"allow_unknown_claim": pol.AllowUnknownClaim,
			"mp_override":         pol.MPOverride,
		},
	}
	// 上次手动拉取时刻（零值不输出）：配置页据此展示目录新鲜度。
	if at := p.lastActivitiesPull(); !at.IsZero() {
		body["synced_at"] = at.Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, body)
}

// ---------------------------------------------------------------------------
// 手动拉取上游活动目录
// ---------------------------------------------------------------------------

// noteActivitiesPulledAt 记录一次成功的手动拉取时刻。
func (p *Panel) noteActivitiesPulledAt(at time.Time) {
	p.activitiesPulledMu.Lock()
	p.activitiesPulledAt = at
	p.activitiesPulledMu.Unlock()
}

// lastActivitiesPull 最近一次手动拉取时刻（零值 = 从未拉取）。
func (p *Panel) lastActivitiesPull() time.Time {
	p.activitiesPulledMu.Lock()
	defer p.activitiesPulledMu.Unlock()
	return p.activitiesPulledAt
}

// activitySyncErr 手动拉取时单账号的失败项（不阻塞其他账号）。
type activitySyncErr struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Error    string `json:"error"`
}

// tasksSyncActivities 手动拉取上游任务活动目录（面板「拉取上游活动」按钮）：
// 并发按账号拉取成长任务列表（默认 + 小程序口径）并入 knownTasks 全景目录，
// 让上游新下发的活动无需先跑一遍待办扫描就出现在「活动自动化配置」页。
//
// 与 tasksScanAll 的分工：扫描按「未完成且可自动化」过滤（回答“现在该做什么”），
// 本接口不做进度/状态过滤（回答“上游一共下发了哪些活动”），并顺带刷新 mp 专属码
// 探测。只读幂等：不 accept、不执行动作；单账号失败只记录（默认与 mp 口径都失败
// 才计入 errors，mp 单侧失败静默——与 pendingGrowthTasks 同口径，避免无 mp 任务的
// 部署刷错误）。
//
// 返回的「新发现」= 目录此前没有、且既非内置动作、也未被显式配置的码——与配置页
// 「未配置新活动」过滤（前端会据此自动切过去）保持同一口径；不自动启用任何自动化。
func (p *Panel) tasksSyncActivities(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Pool == nil || p.cfg.Upstream == nil {
		writeErr(w, http.StatusNotImplemented, "account pool or upstream not available")
		return
	}

	// 拉取前的已知目录快照：用于判定本次「新发现」。
	before := map[string]bool{}
	p.knownTasksMu.RLock()
	for code := range p.knownTasks {
		before[code] = true
	}
	p.knownTasksMu.RUnlock()
	pol := p.autotaskPolicy() // 「新发现」过滤用的策略快照（配置过的码不该再报新）

	type syncAccount struct {
		a    *auth.Auth
		nick string
	}
	var accts []syncAccount
	for _, st := range p.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		// 门控同扫描：global 账号无 CN 成长体系，企业版上游 403
		//（growth system is only available for personal users），均不发上游调用。
		if a.IsGlobal() || a.IsEnterprise() {
			continue
		}
		accts = append(accts, syncAccount{a: a, nick: a.NicknameValue()})
	}

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		sem    = make(chan struct{}, 3) // 与 packages/vouchers 同款限流
		pulled int
		errs   = []activitySyncErr{} // 恒输出 []（不是 null），前端免判空
		union  = map[string]upstream.Task{}
	)
	for _, it := range accts {
		wg.Add(1)
		go func(it syncAccount) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			def, defErr := p.cfg.Upstream.ListTasks(it.a)
			mp, mpErr := p.cfg.Upstream.ListTasksMP(it.a)
			if defErr != nil && mpErr != nil {
				mu.Lock()
				errs = append(errs, activitySyncErr{UID: it.a.UID, Nickname: it.nick, Error: defErr.Error()})
				mu.Unlock()
				return
			}
			// 双口径都成功才记录差集（单侧失败会把全部任务误判成 mp 专属）。
			if defErr == nil && mpErr == nil {
				p.noteMPDetected(mpExclusiveCodes(def, mp))
			}
			p.recordKnownTasks(def)
			p.recordKnownTasks(mp)
			mu.Lock()
			pulled++
			for _, list := range [][]upstream.Task{def, mp} {
				for _, t := range list {
					if t.TaskCode == "" {
						continue
					}
					// mp 列表是默认口径的超集：已有条目只在标题为空时被覆盖，
					// 避免两侧标题/描述不同导致每次拉取结果抖动。
					if prev, ok := union[t.TaskCode]; !ok || prev.Title == "" {
						union[t.TaskCode] = t
					}
				}
			}
			mu.Unlock()
		}(it)
	}
	wg.Wait()

	// 「新发现」= 本次上游下发但此前目录没有、且非内置动作（内置动作恒在配置页）、
	// 也未被显式配置（已配置的码在配置页本就有行，不算新发现——该判定必须与
	// 配置页的「未配置新活动」过滤同口径，否则前端切过去后可能看不到这些码）。
	newCodes := make([]string, 0)
	for code := range union {
		if before[code] || autoActionByCode(code) != nil {
			continue
		}
		if _, configured := pol.Tasks[code]; configured {
			continue
		}
		newCodes = append(newCodes, code)
	}
	sort.Strings(newCodes)
	sort.Slice(errs, func(i, j int) bool { return errs[i].UID < errs[j].UID })

	syncedAt := ""
	if pulled > 0 {
		at := time.Now()
		p.noteActivitiesPulledAt(at)
		syncedAt = at.Format(time.RFC3339)
	}
	log.Printf("panel: 拉取上游活动：账号 %d/%d，活动 %d 项，新发现 %d 项，失败 %d 个",
		pulled, len(accts), len(union), len(newCodes), len(errs))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"accounts":  len(accts),
		"pulled":    pulled,
		"total":     len(union),
		"new":       len(newCodes),
		"new_codes": newCodes,
		"errors":    errs,
		"synced_at": syncedAt,
	})
}

// tasksSavePolicy 更新单个任务的自动化策略并热应用落盘。
func (p *Panel) tasksSavePolicy(w http.ResponseWriter, r *http.Request) {
	if p.cfg.ConfigAPI == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	var body struct {
		Code       string `json:"code"`
		Enabled    *bool  `json:"enabled,omitempty"`
		MP         *bool  `json:"mp,omitempty"`
		ActionType string `json:"action_type,omitempty"`
		Model      string `json:"model,omitempty"`
		Gap        string `json:"gap,omitempty"`
		Target     int64  `json:"target,omitempty"`
		Window     string `json:"window,omitempty"`
		ActivityID string `json:"activity_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}
	code := strings.TrimSpace(body.Code)
	if code == "" {
		writeErr(w, http.StatusBadRequest, "task code required")
		return
	}

	_, err := p.cfg.ConfigAPI.UpdateGrowthAutotasks(func(g *config.GrowthAutotasks) error {
		if g.Tasks == nil {
			g.Tasks = make(map[string]config.GrowthTaskPolicy)
		}
		curr := g.Tasks[code]
		if body.ActionType != "" {
			curr.ActionType = strings.TrimSpace(body.ActionType)
		}
		if body.Model != "" || body.ActionType == "model_chat" {
			curr.Model = strings.TrimSpace(body.Model)
		}
		curr.Gap = strings.TrimSpace(body.Gap)
		curr.Target = body.Target
		curr.Window = strings.TrimSpace(body.Window)
		curr.ActivityID = strings.TrimSpace(body.ActivityID)
		g.Tasks[code] = curr

		// 处理 Enabled 开关
		if body.Enabled != nil {
			if *body.Enabled {
				// 从 Disabled 中移除
				var newDisabled []string
				for _, c := range g.Disabled {
					if c != code {
						newDisabled = append(newDisabled, c)
					}
				}
				g.Disabled = newDisabled
				// 若有白名单且不在白名单中，追加进白名单
				if len(g.Only) > 0 {
					has := false
					for _, c := range g.Only {
						if c == code {
							has = true
							break
						}
					}
					if !has {
						g.Only = append(g.Only, code)
					}
				}
			} else {
				// 禁用：加入 Disabled
				has := false
				for _, c := range g.Disabled {
					if c == code {
						has = true
						break
					}
				}
				if !has {
					g.Disabled = append(g.Disabled, code)
				}
			}
		}

		// 处理 MP 开关
		if body.MP != nil {
			if *body.MP {
				has := false
				for _, c := range g.MPCodes {
					trimmed := strings.TrimPrefix(c, "+")
					if trimmed == code {
						has = true
						break
					}
				}
				if !has {
					g.MPCodes = append(g.MPCodes, "+"+code)
				}
			} else {
				var newMP []string
				for _, c := range g.MPCodes {
					trimmed := strings.TrimPrefix(c, "+")
					if trimmed != code {
						newMP = append(newMP, c)
					}
				}
				g.MPCodes = newMP
			}
		}
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("panel: 任务策略已更新 code=%s action=%s", code, body.ActionType)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "任务配置已更新并热生效"})
}

// tasksQueueStatus 队列状态（轮询用）。
func (p *Panel) tasksQueueStatus(w http.ResponseWriter, r *http.Request) {
	q := p.queue()
	q.mu.Lock()
	defer q.mu.Unlock()
	items := make([]queueItem, len(q.items))
	copy(items, q.items)
	writeJSON(w, http.StatusOK, map[string]any{
		"running":    q.running,
		"total":      len(items),
		"conc":       q.conc,
		"started":    !q.startedAt.IsZero(),
		"started_at": q.startedAt,
		"seq":        q.seq,
		"items":      items,
	})
}

// schoolVouchers 我的券码：逐 CN 账号查开学季 /vouchers（3 并发，与 packages
// 同款限流），失败只在对应账号标 error。global 账号无开学季，不发上游调用。
func (p *Panel) schoolVouchers(w http.ResponseWriter, r *http.Request) {
	accts := p.cfg.Pool.List()
	type row struct {
		UID      string                   `json:"uid"`
		Nickname string                   `json:"nickname"`
		Vouchers []upstream.SchoolVoucher `json:"vouchers"`
		Err      string                   `json:"error,omitempty"`
	}
	out := make([]row, len(accts))
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, st := range accts {
		if st.Disabled {
			continue // 未占位，行末统一压掉
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(i int, a *auth.Auth) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			it := row{UID: a.UID, Nickname: a.NicknameValue()}
			switch {
			case a.IsGlobal():
				it.Err = "global realm（无开学季活动）"
			default:
				vs, err := p.cfg.Upstream.SchoolVouchers(a)
				if err != nil {
					it.Err = err.Error()
				} else {
					it.Vouchers = vs
				}
			}
			out[i] = it
		}(i, a)
	}
	wg.Wait()
	res := make([]row, 0, len(out))
	for _, it := range out {
		if it.UID != "" {
			res = append(res, it)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": res})
}
