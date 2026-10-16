// activity_sync_test.go 「手动拉取上游活动」接口（tasks/sync_activities）的行为守护：
// 目录合并（默认 ∪ mp 口径按 task_code 去重）、账号门控（global / 企业版 / 禁用不发
// 上游）、单账号失败不阻塞其余账号、「新发现」判定（内置动作不算新）、mp 专属码探测
// 与 synced_at 时间戳透出。
//
// 这条链路是「不跑待办扫描也能看到上游新活动」的唯一入口：一旦回归，配置页会重新
// 退回「必须先扫描一次才看得到新活动」，因此口径要与 tasksScanAll 显式对齐。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy_manager/internal/auth"
	"workbuddy_manager/internal/pool"
	"workbuddy_manager/internal/upstream"
)

// newSyncPanel 构造带账号池的假上游面板：两个可拉取 CN 账号 + 一个 global、
// 一个企业版（均不发上游）+ 一个禁用账号（也不发上游）。
func newSyncPanel(t *testing.T, f *fakeGrowthServer) (*Panel, func()) {
	t.Helper()
	p, closeFn := newFakeUpstream(t, f)
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at1", Nickname: "一号"})
	pl.Add(&auth.Auth{UID: "u2", AccessToken: "at2", Nickname: "二号"})
	pl.Add(&auth.Auth{UID: "g1", AccessToken: "ag1", Nickname: "国际号", Domain: "www.workbuddy.ai"})
	pl.Add(&auth.Auth{UID: "e1", AccessToken: "ae1", Nickname: "企业号", EnterpriseID: "ent-1"})
	pl.Add(&auth.Auth{UID: "d1", AccessToken: "ad1", Nickname: "禁用号"})
	pl.Disable("d1", "测试禁用")
	p.cfg.Pool = pl
	return p, closeFn
}

// postSync 调一次拉取接口（测试面板无 APIKey，免鉴权）。
func postSync(t *testing.T, p *Panel) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/tasks/sync_activities", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

type syncResp struct {
	Accounts int      `json:"accounts"`
	Pulled   int      `json:"pulled"`
	Total    int      `json:"total"`
	New      int      `json:"new"`
	NewCodes []string `json:"new_codes"`
	Errors   []struct {
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
		Error    string `json:"error"`
	} `json:"errors"`
	SyncedAt string `json:"synced_at"`
}

// 拉取把两口径任务并入目录：内置动作不算「新发现」，mp 专属码被探测到，
// global/企业/禁用账号不发上游调用。
func TestTasksSyncActivitiesCatalog(t *testing.T) {
	f := &fakeGrowthServer{
		tasksJSON: `[{"task_code":"chat_5","title":"每日对话","target":5,"current":1,"accept_status":"accepted"},` +
			`{"task_code":"Fresh_Task_A","title":"新活动A","target":1,"current":0,"accept_status":"not_accepted"}]`,
		mpJSON: `[{"task_code":"chat_5","title":"每日对话","target":5,"current":1,"accept_status":"accepted"},` +
			`{"task_code":"Fresh_Task_A","title":"新活动A","target":1,"current":0,"accept_status":"not_accepted"},` +
			`{"task_code":"Fresh_MP_Task","title":"小程序新活动","target":1,"current":0,"accept_status":"not_accepted"}]`,
	}
	p, closeFn := newSyncPanel(t, f)
	defer closeFn()

	rec := postSync(t, p)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp syncResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Accounts != 2 || resp.Pulled != 2 {
		t.Fatalf("账号门控不符：accounts=%d pulled=%d errors=%+v", resp.Accounts, resp.Pulled, resp.Errors)
	}
	if resp.Total != 3 {
		t.Fatalf("目录去重后应为 3 项，got %d", resp.Total)
	}
	if resp.New != 2 || len(resp.NewCodes) != 2 {
		t.Fatalf("新发现应为 2 项（内置 chat_5 不算），got %d %v", resp.New, resp.NewCodes)
	}
	if resp.NewCodes[0] != "Fresh_MP_Task" || resp.NewCodes[1] != "Fresh_Task_A" {
		t.Fatalf("新发现码应按字典序：%v", resp.NewCodes)
	}
	if resp.SyncedAt == "" {
		t.Fatal("成功拉取应回填 synced_at")
	}
	if p.lastActivitiesPull().IsZero() {
		t.Fatal("拉取时刻未记录")
	}
	if _, ok := p.getKnownTask("Fresh_Task_A"); !ok {
		t.Fatal("拉取到的活动应并入 knownTasks 目录")
	}
	if !p.isMPTaskCode("Fresh_MP_Task") || p.isMPTaskCode("Fresh_Task_A") {
		t.Fatalf("mp 专属码探测异常：mp=%v regular=%v", p.isMPTaskCode("Fresh_MP_Task"), p.isMPTaskCode("Fresh_Task_A"))
	}

	// 配置页数据源：新活动以「未配置」出现，内置动作仍启用，synced_at 一并透出。
	req := httptest.NewRequest(http.MethodGet, "/panel/api/tasks/actions", nil)
	arec := httptest.NewRecorder()
	p.ServeHTTP(arec, req)
	if arec.Code != http.StatusOK {
		t.Fatalf("actions status=%d body=%s", arec.Code, arec.Body.String())
	}
	var alist struct {
		Actions  []taskActionInfo `json:"actions"`
		SyncedAt string           `json:"synced_at"`
	}
	if err := json.Unmarshal(arec.Body.Bytes(), &alist); err != nil {
		t.Fatalf("unmarshal actions: %v", err)
	}
	if alist.SyncedAt == "" {
		t.Fatal("tasks/actions 应透出最近拉取时刻")
	}
	byCode := map[string]taskActionInfo{}
	for _, a := range alist.Actions {
		byCode[a.Code] = a
	}
	if got, ok := byCode["Fresh_Task_A"]; !ok || !got.Unconfigured || got.Enabled {
		t.Fatalf("新活动应以未配置形态出现：%+v", got)
	}
	if got, ok := byCode["Fresh_MP_Task"]; !ok || !got.Unconfigured || !got.MP {
		t.Fatalf("mp 新活动应带 mp 标记且未配置：%+v", got)
	}
	if got := byCode["chat_5"]; !got.Enabled {
		t.Fatal("内置动作不应受拉取影响")
	}

	// 再次拉取：目录已熟知，不再报「新发现」，但时间戳刷新。
	rec = postSync(t, p)
	var again syncResp
	if err := json.Unmarshal(rec.Body.Bytes(), &again); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if again.New != 0 {
		t.Fatalf("二次拉取不应重复报新发现：%v", again.NewCodes)
	}
	if again.SyncedAt == "" {
		t.Fatal("二次拉取也应刷新 synced_at")
	}
}

// 全账号失败：如实返回 pulled=0 与错误明细，不写 synced_at（避免前端误报已同步）。
func TestTasksSyncActivitiesAllFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	p := New(Config{Version: "test", Upstream: c})
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at1", Nickname: "一号"})
	p.cfg.Pool = pl

	rec := postSync(t, p)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp syncResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Accounts != 1 || resp.Pulled != 0 || len(resp.Errors) != 1 {
		t.Fatalf("全失败应如实上报：%+v", resp)
	}
	if resp.Errors[0].UID != "u1" || resp.Errors[0].Error == "" {
		t.Fatalf("错误明细缺账号或原因：%+v", resp.Errors[0])
	}
	if resp.SyncedAt != "" || !p.lastActivitiesPull().IsZero() {
		t.Fatal("失败不应写入拉取时刻")
	}
}

// 已配置/已屏蔽的码不算「新发现」（配置页本就有行，且不会出现在「未配置新活动」
// 过滤里）；但目录仍要记录，标题/描述可供展示。否则前端“切到未配置”后会看不到
// 报告出来的新码，口径就分叉了。
func TestTasksSyncActivitiesConfiguredNotNew(t *testing.T) {
	f := &fakeGrowthServer{tasksJSON: `[{"task_code":"new_promo_chat","title":"推荐活动","target":3,"current":0},` +
		`{"task_code":"blocked_promo","title":"已屏蔽活动","target":1,"current":0},` +
		`{"task_code":"brand_new","title":"全新活动","target":1,"current":0}]`}
	p, closeFn := newSyncPanel(t, f)
	defer closeFn()
	p.SetAutotaskPolicy(&AutotaskPolicy{
		Disabled: map[string]bool{"blocked_promo": true},
		Tasks: map[string]TaskOption{
			"new_promo_chat": {ActionType: "chat_count"},
			"blocked_promo":  {ActionType: "claim_only"},
		},
	})

	rec := postSync(t, p)
	var resp syncResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Total != 3 {
		t.Fatalf("目录应记录全部 3 项（不论是否已配置），got %d", resp.Total)
	}
	if resp.New != 1 || len(resp.NewCodes) != 1 || resp.NewCodes[0] != "brand_new" {
		t.Fatalf("只有未配置过的码算新发现：%d %v", resp.New, resp.NewCodes)
	}
	for _, code := range []string{"new_promo_chat", "blocked_promo"} {
		if _, ok := p.getKnownTask(code); !ok {
			t.Fatalf("已配置的码 %s 仍应并入目录", code)
		}
	}

	// 配置页：已配置码各出现一次且都不是 unconfigured（与 newCodes 口径一致），
	// 真正未配置的码以 unconfigured 出现（才能被「未配置新活动」过滤看到）。
	req := httptest.NewRequest(http.MethodGet, "/panel/api/tasks/actions", nil)
	arec := httptest.NewRecorder()
	p.ServeHTTP(arec, req)
	var alist struct {
		Actions []taskActionInfo `json:"actions"`
	}
	if err := json.Unmarshal(arec.Body.Bytes(), &alist); err != nil {
		t.Fatalf("unmarshal actions: %v", err)
	}
	count := map[string]int{}
	unconfigured := map[string]bool{}
	for _, a := range alist.Actions {
		count[a.Code]++
		unconfigured[a.Code] = a.Unconfigured
	}
	if count["new_promo_chat"] != 1 || count["blocked_promo"] != 1 {
		t.Fatalf("已配置码不应重复出现：%v", count)
	}
	if unconfigured["new_promo_chat"] || unconfigured["blocked_promo"] {
		t.Fatalf("已配置码不应标为 unconfigured：%v", unconfigured)
	}
	if !unconfigured["brand_new"] {
		t.Fatal("未配置的新码应以 unconfigured 出现")
	}
}

// 单账号失败不阻塞其余账号：失败者进 errors，成功者照常并入目录。
func TestTasksSyncActivitiesPartialFailure(t *testing.T) {
	f := &fakeGrowthServer{tasksJSON: `[{"task_code":"Fresh_Task_B","title":"新活动B","target":1,"current":0}]`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// u2 的两次列表都失败（按 Authorization 头区分账号，无共享可变状态）。
		if r.Header.Get("Authorization") == "Bearer at2" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		f.handler(t).ServeHTTP(w, r)
	}))
	defer srv.Close()
	c := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	p := New(Config{Version: "test", Upstream: c})
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at1", Nickname: "一号"})
	pl.Add(&auth.Auth{UID: "u2", AccessToken: "at2", Nickname: "二号"})
	p.cfg.Pool = pl

	rec := postSync(t, p)
	var resp syncResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Accounts != 2 || resp.Pulled != 1 || len(resp.Errors) != 1 {
		t.Fatalf("部分失败应区分成功/失败账号：%+v", resp)
	}
	if resp.Errors[0].UID != "u2" {
		t.Fatalf("失败账号应为 u2：%+v", resp.Errors)
	}
	if _, ok := p.getKnownTask("Fresh_Task_B"); !ok {
		t.Fatal("成功账号的活动仍应并入目录")
	}
}
