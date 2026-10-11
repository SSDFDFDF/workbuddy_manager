// growth_autotasks_test.go growth.autotasks（成长任务自动化策略）的配置面守护：
// 归一化（trim/去重/空项）、非法值 fail fast、派生字段解析，以及「缺省 = 既有行为」。
//
// 策略层的运行时行为见 internal/panel/autotask_policy_test.go；这里只保证配置面
// 的语义稳定——两处加起来覆盖「可配置化」的完整链路。
package config

import (
	"strings"
	"testing"
	"time"
)

// 缺省：不配置 growth.autotasks 时全部为零值（= 内置行为），且不产生告警。
func TestGrowthAutotasksDefaultZero(t *testing.T) {
	c, err := ParseConfig([]byte(`{}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	g := c.Growth.Autotasks
	if len(g.Disabled) != 0 || len(g.Only) != 0 || len(g.Order) != 0 || len(g.MPCodes) != 0 {
		t.Fatalf("缺省列表应为空：%+v", g)
	}
	if g.AllowUnknownClaim || len(g.Tasks) != 0 {
		t.Fatalf("缺省开关/覆盖应为零值：%+v", g)
	}
	if len(c.Warnings) != 0 {
		t.Fatalf("缺省不应产生告警：%v", c.Warnings)
	}
}

// 归一化：trim、去重、剔空；顺序保持出现序。
func TestGrowthAutotasksNormalizeLists(t *testing.T) {
	c, err := ParseConfig([]byte(`{"growth":{"autotasks":{
	  "disabled":[" chat_5 ","chat_5","", "  ", "skill_1"],
	  "only":["a","b","a"],
	  "order":["template_5"," template_5 "],
	  "mp_codes":[" +Extra_MP ","+Extra_MP","Bare_Code"]
	}}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	g := c.Growth.Autotasks
	if strings.Join(g.Disabled, ",") != "chat_5,skill_1" {
		t.Fatalf("disabled 归一化不符：%v", g.Disabled)
	}
	if strings.Join(g.Only, ",") != "a,b" {
		t.Fatalf("only 归一化不符：%v", g.Only)
	}
	if len(g.Order) != 1 || g.Order[0] != "template_5" {
		t.Fatalf("order 归一化不符：%v", g.Order)
	}
	// "+" 前缀语义保留（"+Extra_MP" 与裸 "Bare_Code" 是两种语义，不算重复）。
	if strings.Join(g.MPCodes, ",") != "+Extra_MP,Bare_Code" {
		t.Fatalf("mp_codes 归一化不符：%v", g.MPCodes)
	}
}

// 逐任务参数：合法值解析成派生字段。
func TestGrowthTaskPolicyDerived(t *testing.T) {
	c, err := ParseConfig([]byte(`{"growth":{"autotasks":{"tasks":{
	  " Sequential_Tasks_3 ":{"gap":"12s","target":7,"attempt":true},
	  "black_cat":{"window":"23:00-08:00"},
	  "school_season":{"activity_id":" school_open_day_2027 "}
	}}}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tasks := c.Growth.Autotasks.Tasks
	p, ok := tasks["Sequential_Tasks_3"] // 键 trim
	if !ok {
		t.Fatalf("任务码未 trim：%v", tasks)
	}
	if p.GapDur != 12*time.Second || p.Gap != "12s" {
		t.Fatalf("gap 派生不符：%+v", p)
	}
	if p.Target != 7 || p.Attempt == nil || !*p.Attempt {
		t.Fatalf("target/attempt 不符：%+v", p)
	}
	w := tasks["black_cat"]
	if !w.HasWindow || w.WindowStartMin != 23*60 || w.WindowEndMin != 8*60 {
		t.Fatalf("window 派生不符：%+v", w)
	}
	e := tasks["school_season"]
	if e.ActivityID != "school_open_day_2027" {
		t.Fatalf("activity_id 未 trim：%q", e.ActivityID)
	}
}

// 非法值一律 fail fast（不静默回落成另一个行为），错误信息带完整字段路径。
func TestGrowthAutotasksRejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		json string
		want string
	}{
		{"gap 非法", `{"growth":{"autotasks":{"tasks":{"chat_5":{"gap":"45"}}}}}`, "growth.autotasks.tasks.chat_5.gap"},
		{"gap 负值", `{"growth":{"autotasks":{"tasks":{"chat_5":{"gap":"-1s"}}}}}`, "growth.autotasks.tasks.chat_5.gap"},
		{"target 负值", `{"growth":{"autotasks":{"tasks":{"chat_5":{"target":-2}}}}}`, "growth.autotasks.tasks.chat_5.target"},
		{"window 缺段", `{"growth":{"autotasks":{"tasks":{"black_cat":{"window":"23:00"}}}}}`, "growth.autotasks.tasks.black_cat.window"},
		{"window 小时越界", `{"growth":{"autotasks":{"tasks":{"black_cat":{"window":"24:00-08:00"}}}}}`, "growth.autotasks.tasks.black_cat.window"},
		{"window 分钟越界", `{"growth":{"autotasks":{"tasks":{"black_cat":{"window":"23:60-08:00"}}}}}`, "growth.autotasks.tasks.black_cat.window"},
		{"window 非数字", `{"growth":{"autotasks":{"tasks":{"black_cat":{"window":"abc-def"}}}}}`, "growth.autotasks.tasks.black_cat.window"},
		{"mp_codes 空前缀", `{"growth":{"autotasks":{"mp_codes":["+"]}}}`, "growth.autotasks.mp_codes"},
		{"tasks 空码", `{"growth":{"autotasks":{"tasks":{"":{}}}}}`, "growth.autotasks.tasks"},
		{"tasks 去空白后重名", `{"growth":{"autotasks":{"tasks":{"chat_5":{}," chat_5 ":{"gap":"3s"}}}}}`, "重复的任务码"},
		{"action_type 非法", `{"growth":{"autotasks":{"tasks":{"chat_5":{"action_type":"invalid_type"}}}}}`, "growth.autotasks.tasks.chat_5.action_type"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(c.json))
			if err == nil {
				t.Fatal("期望报错，实际通过")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误信息 %q 未含字段路径 %q", err.Error(), c.want)
			}
		})
	}
}

// 跨零点窗口与「全天」窗口的边界取值合法。
func TestGrowthWindowBoundaries(t *testing.T) {
	c, err := ParseConfig([]byte(`{"growth":{"autotasks":{"tasks":{
	  "a":{"window":"00:00-00:00"},
	  "b":{"window":"23:59-00:00"}
	}}}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a := c.Growth.Autotasks.Tasks["a"]; !a.HasWindow || a.WindowStartMin != 0 || a.WindowEndMin != 0 {
		t.Fatalf("全天窗口解析不符：%+v", a)
	}
	if b := c.Growth.Autotasks.Tasks["b"]; b.WindowStartMin != 23*60+59 || b.WindowEndMin != 0 {
		t.Fatalf("跨零点窗口解析不符：%+v", b)
	}
}

// 目录登记：growth.* 必须被 catalog 覆盖为 Hot（守护测试已全量校验，这里补语义断言）。
func TestGrowthAutotasksCatalogHot(t *testing.T) {
	cat := Entries()
	for _, path := range []string{
		"growth.autotasks.disabled", "growth.autotasks.only", "growth.autotasks.order",
		"growth.autotasks.mp_codes", "growth.autotasks.allow_unknown_claim",
		"growth.autotasks.tasks.chat_5.gap", "growth.autotasks.tasks.black_cat.window",
	} {
		f, ok := cat.Lookup(path)
		if !ok {
			t.Fatalf("%s 未在 catalog 登记", path)
		}
		if f.Mode != Hot {
			t.Fatalf("%s 应为热生效，got %v", path, f.Mode)
		}
	}
}

// 差异计算认 growth.* 叶子（面板保存时据此提示；热字段不应出现在需重启清单）。
func TestGrowthAutotasksDiffPaths(t *testing.T) {
	old := Default()
	cur, err := ParseConfig([]byte(`{"growth":{"autotasks":{"tasks":{"chat_5":{"gap":"3s"}}}}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	diff := DiffConfig(old, cur)
	if !diff.Has("growth.autotasks.tasks.chat_5.gap") {
		t.Fatalf("差异未包含 gap：%v", diff.Slice())
	}
	// 热字段：不落到需重启（catalog 判定）。
	cat := Entries()
	for _, p := range diff.Slice() {
		if cat.IsRestartPath(p) {
			t.Fatalf("growth 字段不应是重启项：%s", p)
		}
	}
}
