// autotask_policy_test.go 成长任务自动化策略层的行为守护：
// 启用集合（黑/白名单）、执行顺序、逐任务参数覆盖、mp 口径解析（内置 ∪ 配置 ∪
// 运行时探测）、计数窗口与「未内置判据」的兜底动作语义。
//
// 这一层是「哪些活动能自动化」可配置化的关键：判据实现仍是代码，但启用集合与
// 参数不该再改代码。因此每个开关都要有用例，且默认（零值策略）必须与老行为逐字一致。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy_manager/internal/auth"
	"workbuddy_manager/internal/config"
	"workbuddy_manager/internal/pool"
	"workbuddy_manager/internal/upstream"
)

// newPolicyPanel 构造只带策略的空面板（不接上游/池，策略层是纯函数）。
func newPolicyPanel(pol *AutotaskPolicy) *Panel {
	p := New(Config{Version: "test"})
	if pol != nil {
		p.SetAutotaskPolicy(pol)
	}
	return p
}

func boolPtr(v bool) *bool { return &v }

// 默认策略 = 既有行为：内置动作全启用，未知任务不参与。
func TestActionForDefaultPolicy(t *testing.T) {
	p := newPolicyPanel(nil)
	for _, code := range []string{"chat_5", "school_season", "Sequential_Tasks_3", "black_cat"} {
		if act := p.actionFor(code); act == nil {
			t.Fatalf("默认策略下 %s 应有动作", code)
		}
	}
	if act := p.actionFor("No_Such_Task"); act != nil {
		t.Fatalf("未内置任务默认不参与自动化，got %+v", act)
	}
	if act := p.actionFor("(批量接受)"); act != nil {
		t.Fatalf("内部伪码不应被当成任务")
	}
}

// 黑/白名单：Disabled 优先于 Only；空白名单 = 不启用白名单。
func TestActionForEnableSets(t *testing.T) {
	p := newPolicyPanel(&AutotaskPolicy{Disabled: map[string]bool{"chat_5": true}})
	if p.actionFor("chat_5") != nil {
		t.Fatal("黑名单任务不应有动作")
	}
	if p.actionFor("first_buddy") == nil {
		t.Fatal("未屏蔽任务应保留动作")
	}

	p = newPolicyPanel(&AutotaskPolicy{
		Only:     map[string]bool{"chat_5": true, "first_buddy": true},
		HasOnly:  true,
		Disabled: map[string]bool{"chat_5": true},
	})
	if p.actionFor("chat_5") != nil {
		t.Fatal("同时命中黑白名单时黑名单优先")
	}
	if p.actionFor("first_buddy") == nil {
		t.Fatal("白名单内任务应保留动作")
	}
	if p.actionFor("template_5") != nil {
		t.Fatal("白名单外任务不应参与自动化")
	}

	empty := newPolicyPanel(&AutotaskPolicy{Only: map[string]bool{}, HasOnly: false})
	if empty.actionFor("template_5") == nil {
		t.Fatal("空策略（HasOnly=false）不应屏蔽")
	}
}

// 未知任务兜底：allow_unknown_claim 只合成「accept + 达标领奖」，且标 claimOnly。
func TestActionForUnknownClaim(t *testing.T) {
	p := newPolicyPanel(&AutotaskPolicy{AllowUnknownClaim: true})
	act := p.actionFor("New_Activity_2027")
	if act == nil || !act.claimOnly {
		t.Fatalf("allow_unknown_claim 应合成 claimOnly 动作，got %+v", act)
	}
	if act.run == nil {
		t.Fatal("兜底动作必须有 run")
	}
	if act := p.actionFor("(批量接受)"); act != nil {
		t.Fatalf("伪码不应合成兜底动作，got %+v", act)
	}
}

// 动态动作模板：对未内置的任务配置 action_type，能动态派生对应执行器并入待办。
func TestActionForDynamicTemplates(t *testing.T) {
	p := newPolicyPanel(&AutotaskPolicy{
		Tasks: map[string]TaskOption{
			"custom_chat_10": {
				ActionType: "chat_count",
				Target:     10,
			},
			"custom_deepseek": {
				ActionType: "model_chat",
				Model:      "deepseek-chat",
			},
			"custom_mp_act": {
				ActionType: "mp_chat",
				ActivityID: "summer_camp_2026",
			},
		},
	})

	// chat_count 动态动作
	actChat := p.actionFor("custom_chat_10")
	if actChat == nil || actChat.claimOnly || actChat.run == nil {
		t.Fatalf("chat_count 模板动作生成失败: %+v", actChat)
	}
	if !strings.Contains(actChat.Desc, "对话活跃上报") {
		t.Errorf("chat_count 描述不符: %s", actChat.Desc)
	}

	// model_chat 动态动作
	actModel := p.actionFor("custom_deepseek")
	if actModel == nil || actModel.claimOnly || actModel.run == nil {
		t.Fatalf("model_chat 模板动作生成失败: %+v", actModel)
	}
	if !strings.Contains(actModel.Desc, "deepseek-chat") {
		t.Errorf("model_chat 描述未包含模型名: %s", actModel.Desc)
	}

	// mp_chat 动态动作
	actMP := p.actionFor("custom_mp_act")
	if actMP == nil || actMP.claimOnly || actMP.run == nil {
		t.Fatalf("mp_chat 模板动作生成失败: %+v", actMP)
	}

	// 动态动作不受 claimOnly 约束：即使当前进度 0/10 也应入待办队列
	task := upstream.Task{TaskCode: "custom_chat_10", Target: 10, Current: 0}
	if !p.growthPending(task) {
		t.Fatal("配置了动态动作模板的任务在未达标时也应入待办队列")
	}
}

// claimOnly 动作只在任务已达标时进入待办（避免每天白跑一轮 accept）。
func TestGrowthPendingClaimOnly(t *testing.T) {
	p := newPolicyPanel(&AutotaskPolicy{AllowUnknownClaim: true})
	cases := []struct {
		name string
		task upstream.Task
		want bool
	}{
		{"未达标不入队", upstream.Task{TaskCode: "New_X", Target: 3, Current: 1}, false},
		{"进度达标入队", upstream.Task{TaskCode: "New_X", Target: 3, Current: 3}, true},
		{"上游标记可领入队", upstream.Task{TaskCode: "New_X", Claimable: true}, true},
		{"已完成未领入队", upstream.Task{TaskCode: "New_X", AcceptStatus: "completed"}, true},
		{"已领取不入队", upstream.Task{TaskCode: "New_X", Target: 1, Current: 1, Claimed: true}, false},
		{"锁定不入队", upstream.Task{TaskCode: "New_X", Target: 1, Current: 1, Locked: true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := p.growthPending(c.task); got != c.want {
				t.Fatalf("growthPending=%v want %v", got, c.want)
			}
		})
	}
	// 内置动作不受 claimOnly 约束：未达标也入队（动作自己会补进度）。
	if !p.growthPending(upstream.Task{TaskCode: "chat_5", Target: 5, Current: 0}) {
		t.Fatal("内置动作未达标应入队")
	}
	// 未开启兜底时未知任务不入队。
	q := newPolicyPanel(nil)
	if q.growthPending(upstream.Task{TaskCode: "New_X", Target: 1, Current: 1}) {
		t.Fatal("未开启 allow_unknown_claim 时未知任务不应入队")
	}
}

// 顺序覆盖：Order 内的码按列表序在前，未列出的按内置依赖序排在其后。
func TestActionOrderAndIndex(t *testing.T) {
	p := newPolicyPanel(&AutotaskPolicy{Order: []string{"template_5", "chat_5"}})
	got := p.orderedCodes()
	if got[0] != "template_5" || got[1] != "chat_5" {
		t.Fatalf("Order 前置项不符：%v", got[:2])
	}
	if p.actionIndex("template_5") >= p.actionIndex("chat_5") {
		t.Fatal("Order 内顺序应保持列表序")
	}
	if p.actionIndex("chat_5") >= p.actionIndex("first_buddy") {
		t.Fatal("未列出项应排在 Order 之后")
	}
	// Order 里的未知码被忽略且不影响其余排序。
	q := newPolicyPanel(&AutotaskPolicy{Order: []string{"No_Such"}})
	if idx := q.actionIndex("chat_5"); idx >= 1<<20 {
		t.Fatalf("未知 Order 不应把内置任务挤到末位：idx=%d", idx)
	}
}

// actions 返回策略过滤 + 顺序后的动作表（供「一键完成」遍历）。
func TestActionsFilteredByPolicy(t *testing.T) {
	p := newPolicyPanel(&AutotaskPolicy{Disabled: map[string]bool{"chat_5": true}})
	for _, act := range p.actions() {
		if act.TaskCode == "chat_5" {
			t.Fatal("actions() 不应包含被屏蔽任务")
		}
	}
	if len(p.actions()) != len(autoActions)-1 {
		t.Fatalf("actions() 数量=%d want %d", len(p.actions()), len(autoActions)-1)
	}
}

// 逐任务参数覆盖：gap / target / attempt / window / activity_id。
func TestTaskOptionOverrides(t *testing.T) {
	p := newPolicyPanel(&AutotaskPolicy{Tasks: map[string]TaskOption{
		"Sequential_Tasks_3": {Gap: 12 * time.Second, Target: 7, Attempt: boolPtr(true)},
		"black_cat":          {HasWindow: true, WindowStartMin: 22*60 + 30, WindowEndMin: 7 * 60},
		"school_season":      {ActivityID: "school_open_day_2027"},
	}})
	if got := p.gapFor("Sequential_Tasks_3", 45*time.Second); got != 12*time.Second {
		t.Fatalf("gap 覆盖=%v", got)
	}
	if got := p.gapFor("chat_5", 45*time.Second); got != 45*time.Second {
		t.Fatalf("无覆盖应回落内置：%v", got)
	}
	if got := p.targetFor("Sequential_Tasks_3", 5); got != 7 {
		t.Fatalf("target 覆盖=%v", got)
	}
	if got := p.targetFor("chat_5", 5); got != 5 {
		t.Fatalf("target 无覆盖应回落：%v", got)
	}
	if act := p.actionFor("Sequential_Tasks_3"); act == nil || !act.Attempt {
		t.Fatalf("attempt 覆盖未生效：%+v", act)
	}
	if got := p.activityIDFor("school_season"); got != "school_open_day_2027" {
		t.Fatalf("activity_id 覆盖=%v", got)
	}
	if got := p.activityIDFor("chat_5"); got != "" {
		t.Fatalf("无覆盖应为空（用内置默认）：%q", got)
	}
	// 窗口：22:30-07:30（跨零点）
	in := func(h, m int) bool {
		return p.inCountWindow("black_cat", time.Date(2026, 10, 10, h, m, 0, 0, time.Local))
	}
	if !in(23, 0) || !in(6, 0) || in(12, 0) {
		t.Fatal("自定义跨零点窗口判定不符")
	}
}

// 默认计数窗口 23:00–08:00（与上游口径一致；零值策略不得改变老行为）。
func TestDefaultCountWindow(t *testing.T) {
	p := newPolicyPanel(nil)
	in := func(h, m int) bool {
		return p.inCountWindow("black_cat", time.Date(2026, 10, 10, h, m, 0, 0, time.Local))
	}
	for _, c := range []struct {
		h, m int
		want bool
	}{{23, 0, true}, {0, 0, true}, {7, 59, true}, {8, 0, false}, {12, 0, false}, {22, 59, false}} {
		if got := in(c.h, c.m); got != c.want {
			t.Fatalf("%02d:%02d inWindow=%v want %v", c.h, c.m, got, c.want)
		}
	}
}

// 窗口边界语义（经面板侧 inCountWindow 走 upstream.InWindow 实现）：
// start == end = 全天；非跨零点窗口左闭右开。
func TestMinuteInWindow(t *testing.T) {
	p := newPolicyPanel(&AutotaskPolicy{Tasks: map[string]TaskOption{
		"black_cat": {HasWindow: true, WindowStartMin: 10 * 60, WindowEndMin: 10 * 60},
	}})
	at := func(h, m int) bool {
		return p.inCountWindow("black_cat", time.Date(2026, 10, 10, h, m, 0, 0, time.Local))
	}
	if !at(0, 0) || !at(23, 59) {
		t.Fatal("start==end 应为全天")
	}
	q := newPolicyPanel(&AutotaskPolicy{Tasks: map[string]TaskOption{
		"black_cat": {HasWindow: true, WindowStartMin: 0, WindowEndMin: 60},
	}})
	at2 := func(h, m int) bool {
		return q.inCountWindow("black_cat", time.Date(2026, 10, 10, h, m, 0, 0, time.Local))
	}
	if at2(1, 0) || !at2(0, 59) {
		t.Fatal("非跨零点窗口应为左闭右开")
	}
}

// mp 口径解析：内置表默认生效；裸码整体覆盖；"+码" 追加；运行时探测始终并入。
func TestMPCodeResolution(t *testing.T) {
	// 默认：内置表生效。
	p := newPolicyPanel(nil)
	if !p.isMPTaskCode("Sequential_Tasks_1") || p.isMPTaskCode("chat_5") {
		t.Fatal("默认应使用内置 mp 码表")
	}
	// 追加：内置 + 配置。
	p = newPolicyPanel(&AutotaskPolicy{MPCodes: map[string]bool{"New_MP": true}})
	if !p.isMPTaskCode("Sequential_Tasks_1") || !p.isMPTaskCode("New_MP") || p.isMPTaskCode("chat_5") {
		t.Fatal("追加语义不符")
	}
	// 覆盖：只有配置里的码 + 运行时探测。
	p = newPolicyPanel(&AutotaskPolicy{MPCodes: map[string]bool{"New_MP": true}, MPOverride: true})
	if p.isMPTaskCode("Sequential_Tasks_1") {
		t.Fatal("裸码应整体覆盖内置表")
	}
	if !p.isMPTaskCode("New_MP") {
		t.Fatal("覆盖表内的码应生效")
	}
	// 运行时探测：即使覆盖也会并入。
	p.noteMPDetected([]string{"Dynamic_MP"})
	if !p.isMPTaskCode("Dynamic_MP") {
		t.Fatal("运行时探测的 mp 码应并入")
	}
	// 探测过期后失效。
	old := mpDetectTTL
	mpDetectTTL = 0
	defer func() { mpDetectTTL = old }()
	if p.isMPTaskCode("Dynamic_MP") {
		t.Fatal("探测结果过期后不应再命中")
	}
}

// mpExclusiveCodes：mp 列表相对默认列表的差集。
func TestMPExclusiveCodes(t *testing.T) {
	def := []upstream.Task{{TaskCode: "chat_5"}, {TaskCode: "school_season"}}
	mp := []upstream.Task{{TaskCode: "chat_5"}, {TaskCode: "school_season"}, {TaskCode: "Sequential_Tasks_1"}}
	got := mpExclusiveCodes(def, mp)
	if len(got) != 1 || got[0] != "Sequential_Tasks_1" {
		t.Fatalf("差集=%v", got)
	}
	if len(mpExclusiveCodes(nil, nil)) != 0 {
		t.Fatal("空列表差集应为空")
	}
	// 默认列表为空（可能是上游瞬时异常）：不记录差集，避免把常规任务误判成 mp 专属。
	if got := mpExclusiveCodes(nil, []upstream.Task{{TaskCode: "chat_5"}}); got != nil {
		t.Fatalf("默认列表为空时不应记录 mp 专属码：%v", got)
	}
}

// 配置 → 策略：normalize 后的 Config 应完整映射（含 "+" 前缀语义与派生字段）。
func TestNewAutotaskPolicyFromConfig(t *testing.T) {
	cfg, err := config.ParseConfig([]byte(`{
	  "growth": {"autotasks": {
	    "disabled": ["skill_1"],
	    "only": ["skill_1", "chat_5"],
	    "order": ["chat_5"],
	    "mp_codes": ["+Extra_MP"],
	    "allow_unknown_claim": true,
	    "tasks": {
	      "Sequential_Tasks_3": {"gap": "12s", "target": 7, "attempt": true},
	      "black_cat": {"window": "22:30-07:30"},
	      "school_season": {"activity_id": "school_open_day_2027"}
	    }
	  }}
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pol := NewAutotaskPolicy(cfg)
	if !pol.HasOnly || !pol.Disabled["skill_1"] || !pol.Only["chat_5"] {
		t.Fatalf("启用集合映射不符：%+v", pol)
	}
	if pol.MPOverride || !pol.MPCodes["Extra_MP"] {
		t.Fatalf("'+' 前缀应为追加语义：%+v", pol)
	}
	if !pol.AllowUnknownClaim || len(pol.Order) != 1 || pol.Order[0] != "chat_5" {
		t.Fatalf("顶层布尔/顺序映射不符：%+v", pol)
	}
	ov := pol.Tasks["Sequential_Tasks_3"]
	if ov.Gap != 12*time.Second || ov.Target != 7 || ov.Attempt == nil || !*ov.Attempt {
		t.Fatalf("任务参数覆盖映射不符：%+v", ov)
	}
	if w := pol.Tasks["black_cat"]; !w.HasWindow || w.WindowStartMin != 22*60+30 || w.WindowEndMin != 7*60+30 {
		t.Fatalf("窗口派生字段不符：%+v", w)
	}
	if id := pol.Tasks["school_season"].ActivityID; id != "school_open_day_2027" {
		t.Fatalf("activity_id 不符：%q", id)
	}

	// 接线验证：策略注入后过滤/覆盖立即生效。
	p := newPolicyPanel(pol)
	if p.actionFor("skill_1") != nil {
		t.Fatal("only 之外的 skill_1 应被屏蔽")
	}
	if p.actionFor("template_5") != nil {
		t.Fatal("only 之外的 template_5 应被屏蔽")
	}
	if act := p.actionFor("chat_5"); act == nil {
		t.Fatal("only 内的 chat_5 应可用")
	}
	if got := p.gapFor("Sequential_Tasks_3", 45*time.Second); got != 12*time.Second {
		t.Fatalf("配置 gap 未生效：%v", got)
	}
}

// 逐任务 attempt 覆盖只影响副本，不污染全局注册表（多次调用结果稳定）。
func TestAttemptOverrideDoesNotMutateRegistry(t *testing.T) {
	p := newPolicyPanel(&AutotaskPolicy{Tasks: map[string]TaskOption{
		"black_cat": {Attempt: boolPtr(false)},
	}})
	act := p.actionFor("black_cat")
	if act == nil || act.Attempt {
		t.Fatalf("attempt 覆盖=false 未生效：%+v", act)
	}
	q := newPolicyPanel(nil)
	if got := q.actionFor("black_cat"); got == nil || !got.Attempt {
		t.Fatalf("全局注册表被污染：%+v", got)
	}
}

// SetAutotaskPolicy(nil) 回到默认（配置清空后行为不残留）。
func TestSetAutotaskPolicyNilResets(t *testing.T) {
	p := newPolicyPanel(&AutotaskPolicy{Disabled: map[string]bool{"chat_5": true}})
	if p.actionFor("chat_5") != nil {
		t.Fatal("屏蔽未生效")
	}
	p.SetAutotaskPolicy(nil)
	if p.actionFor("chat_5") == nil {
		t.Fatal("置 nil 应回到默认（全启用）")
	}
}

// ---------------------------------------------------------------------------
// 端到端（假上游）：策略如何决定「扫到什么、执行什么」
// ---------------------------------------------------------------------------

// fakeGrowthServer 假上游：growth 任务列表（默认 + mp 口径）与 Web 领奖。
// tasksJSON 为默认口径列表，mpTasksJSON 为空时沿用默认（mp 列表是超集的口径）。
type fakeGrowthServer struct {
	mu        sync.Mutex
	tasksJSON string
	mpJSON    string
	claims    []string // 收到的领奖 task_code（路径）
	accepts   []string // 收到的 accept body
}

func (f *fakeGrowthServer) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/activity/growth/tasks" && r.Method == http.MethodGet:
			body := f.tasksJSON
			if r.Header.Get("X-Client-Platform") == "miniprogram" && f.mpJSON != "" {
				body = f.mpJSON
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"tasks":` + body + `}}`))
		case r.URL.Path == "/v2/activity/growth/tasks/accept":
			f.mu.Lock()
			f.accepts = append(f.accepts, r.URL.Path)
			f.mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasPrefix(r.URL.Path, "/activity/growth/tasks/") && strings.HasSuffix(r.URL.Path, "/claim"):
			code := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/activity/growth/tasks/"), "/claim")
			f.mu.Lock()
			f.claims = append(f.claims, code)
			f.mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"credit":200,"energy":5}}`))
		default:
			t.Errorf("未预期的上游请求：%s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func newFakeUpstream(t *testing.T, f *fakeGrowthServer) (*Panel, func()) {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	c := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	p := New(Config{Version: "test", Upstream: c})
	return p, srv.Close
}

// 扫描待办：被策略屏蔽的任务不出现，其余照常（这是「哪些活动参与自动化」的入口）。
func TestPendingGrowthTasksHonorsPolicy(t *testing.T) {
	f := &fakeGrowthServer{tasksJSON: `[
	  {"task_code":"chat_5","target":5,"current":1,"accept_status":"accepted"},
	  {"task_code":"skill_1","target":1,"current":0,"accept_status":"accepted"}
	]`}
	p, closeFn := newFakeUpstream(t, f)
	defer closeFn()

	pending, err := p.pendingGrowthTasks(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("pendingGrowthTasks: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("默认策略应扫到 2 项，got %d", len(pending))
	}

	p.SetAutotaskPolicy(&AutotaskPolicy{Disabled: map[string]bool{"skill_1": true}})
	pending, err = p.pendingGrowthTasks(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("pendingGrowthTasks: %v", err)
	}
	if len(pending) != 1 || pending[0].TaskCode != "chat_5" {
		t.Fatalf("黑名单未生效：%+v", pending)
	}

	p.SetAutotaskPolicy(&AutotaskPolicy{Only: map[string]bool{"skill_1": true}, HasOnly: true})
	pending, _ = p.pendingGrowthTasks(&auth.Auth{AccessToken: "at", UID: "u1"})
	if len(pending) != 1 || pending[0].TaskCode != "skill_1" {
		t.Fatalf("白名单未生效：%+v", pending)
	}
}

// 兜底动作端到端：未内置判据的任务，达标时 accept + 领奖（不报事件），未达标不动作。
func TestRunClaimOnlyEndToEnd(t *testing.T) {
	f := &fakeGrowthServer{tasksJSON: `[{"task_code":"New_Task","target":3,"current":3,"accept_status":"accepted"}]`}
	p, closeFn := newFakeUpstream(t, f)
	defer closeFn()
	p.SetAutotaskPolicy(&AutotaskPolicy{AllowUnknownClaim: true})

	msg, err := p.runClaimOnly(&auth.Auth{AccessToken: "at", UID: "u1"}, "New_Task")
	if err != nil {
		t.Fatalf("runClaimOnly: %v", err)
	}
	if !strings.Contains(msg, "+200c +5e") {
		t.Fatalf("达标任务应领奖：%q", msg)
	}
	if len(f.claims) != 1 || f.claims[0] != "New_Task" {
		t.Fatalf("领奖请求不符：%v", f.claims)
	}

	// 未达标：不领奖，仅确保已接受（accept 幂等重放）。
	f2 := &fakeGrowthServer{tasksJSON: `[{"task_code":"New_Task","target":3,"current":1,"accept_status":"accepted"}]`}
	p2, close2 := newFakeUpstream(t, f2)
	defer close2()
	p2.SetAutotaskPolicy(&AutotaskPolicy{AllowUnknownClaim: true})
	msg, err = p2.runClaimOnly(&auth.Auth{AccessToken: "at", UID: "u1"}, "New_Task")
	if err != nil {
		t.Fatalf("runClaimOnly: %v", err)
	}
	if !strings.Contains(msg, "进度未达标") {
		t.Fatalf("未达标不应领奖：%q", msg)
	}
	if len(f2.claims) != 0 {
		t.Fatalf("未达标却发出了领奖请求：%v", f2.claims)
	}
}

// mp 口径路由：探测到的 mp 专属码回读/领奖走 mp 列表（配置无需登记）。
func TestMPDetectedRouting(t *testing.T) {
	f := &fakeGrowthServer{
		tasksJSON: `[{"task_code":"chat_5","target":5,"current":1,"accept_status":"accepted"}]`,
		mpJSON:    `[{"task_code":"chat_5","target":5,"current":1,"accept_status":"accepted"},{"task_code":"New_MP_Task","target":1,"current":0,"accept_status":"accepted"}]`,
	}
	p, closeFn := newFakeUpstream(t, f)
	defer closeFn()
	if p.isMPTaskCode("New_MP_Task") {
		t.Fatal("探测前不应认识未登记的 mp 码")
	}
	if _, err := p.pendingGrowthTasks(&auth.Auth{AccessToken: "at", UID: "u1"}); err != nil {
		t.Fatalf("pendingGrowthTasks: %v", err)
	}
	if !p.isMPTaskCode("New_MP_Task") {
		t.Fatal("双口径列表差集应被记录为 mp 专属码")
	}
	if p.isMPTaskCode("chat_5") {
		t.Fatal("两侧都下发的码不应被判为 mp 专属")
	}
}

// 动作清单接口：后端下发启停状态，前端不硬编码任务码。
func TestTasksActionsEndpoint(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k"})
	p.SetAutotaskPolicy(&AutotaskPolicy{Disabled: map[string]bool{"chat_5": true}})
	req := httptest.NewRequest(http.MethodGet, "/panel/api/tasks/actions", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Actions []taskActionInfo `json:"actions"`
		Policy  map[string]any   `json:"policy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Actions) != len(autoActions) {
		t.Fatalf("动作数=%d want %d", len(resp.Actions), len(autoActions))
	}
	byCode := map[string]taskActionInfo{}
	for _, a := range resp.Actions {
		byCode[a.Code] = a
	}
	if byCode["chat_5"].Enabled {
		t.Fatal("被屏蔽任务应标记 enabled=false")
	}
	if !byCode["first_buddy"].Enabled {
		t.Fatal("未屏蔽任务应 enabled=true")
	}
	if byCode["Sequential_Tasks_1"].MP != true {
		t.Fatal("mp 专属码应带 mp 标记")
	}
}

// 「一键完成」对策略屏蔽的任务明确 501（区分「无接口实现」与「被配置屏蔽」），
// 不静默失败、也不误报成需要客户端交互。
func TestAccountTaskAutoBlockedByPolicy(t *testing.T) {
	pl := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", Nickname: "测试号"}
	pl.Add(a)
	p := New(Config{Version: "test", APIKey: "k", Pool: pl})
	p.SetAutotaskPolicy(&AutotaskPolicy{Disabled: map[string]bool{"chat_5": true}})

	post := func(code string) *httptest.ResponseRecorder {
		t.Helper()
		body := strings.NewReader(`{"task_code":"` + code + `"}`)
		req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/u1/tasks/auto", body)
		req.Header.Set("Authorization", "Bearer k")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}
	rec := post("chat_5")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("被屏蔽任务应 501，got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "屏蔽") {
		t.Fatalf("错误信息应说明被策略屏蔽：%s", rec.Body.String())
	}
	// 未内置判据且未开启兜底：501 且提示需要客户端交互。
	rec = post("Some_Unknown_Task")
	if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body.String(), "客户端") {
		t.Fatalf("未实现任务应 501 并提示客户端交互，got %d body=%s", rec.Code, rec.Body.String())
	}
}

// 任务策略保存接口：支持更新单个任务的动作模板与参数并热生效。
func TestTasksSavePolicyEndpoint(t *testing.T) {
	currentCfg := config.Default()
	cfgAPI := config.NewAPI(config.APIConfig{
		LoadConfig: func() (any, error) {
			return currentCfg, nil
		},
		SaveConfig: func(raw []byte) ([]string, error) {
			parsed, err := config.ParseConfig(raw)
			if err != nil {
				return nil, err
			}
			currentCfg = parsed
			return []string{}, nil
		},
	})
	p := New(Config{Version: "test", APIKey: "k", ConfigAPI: cfgAPI})
	p.SetAutotaskPolicy(NewAutotaskPolicy(currentCfg))

	body := strings.NewReader(`{
		"code": "new_promo_chat",
		"enabled": true,
		"action_type": "chat_count",
		"target": 8,
		"gap": "5s"
	}`)
	req := httptest.NewRequest(http.MethodPost, "/panel/api/tasks/policy", body)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 验证配置已更新
	if currentCfg.Growth.Autotasks.Tasks["new_promo_chat"].ActionType != "chat_count" {
		t.Fatalf("ActionType 未更新: %+v", currentCfg.Growth.Autotasks.Tasks["new_promo_chat"])
	}
	if currentCfg.Growth.Autotasks.Tasks["new_promo_chat"].Target != 8 {
		t.Fatalf("Target 未更新: %+v", currentCfg.Growth.Autotasks.Tasks["new_promo_chat"])
	}

	// 验证热应用快照立即识别该任务动作
	p.SetAutotaskPolicy(NewAutotaskPolicy(currentCfg))
	act := p.actionFor("new_promo_chat")
	if act == nil || act.run == nil {
		t.Fatalf("热应用后动作未生成: %+v", act)
	}
}
