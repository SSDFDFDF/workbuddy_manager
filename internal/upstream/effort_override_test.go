package upstream

import "testing"

// 本地默认档覆盖（upstream.effort_default_overrides）是「本地的覆盖上游配置」的唯一
// 落点：命中即压过远端 defaultEffort 与静态兜底默认档，且必须先落到该模型的支持集上——
// /v1/models 宣告的档位与出站可执行的档位永远是同一份答案。
func TestEffortDefaultOverridePriorityAndWildcard(t *testing.T) {
	resetEffortDefaultOverrides()
	t.Cleanup(resetEffortDefaultOverrides)
	SetEffortDefaultOverrides(map[string]string{
		"*":              "medium",
		"cn:*":           "high",
		"Cn:GLM-5.2":     "xhigh", // 大小写归一：键面声明什么写法都能命中
		"glm-5.2":        "low",
		"global:hy3":     "max",
		"  spaced  ":     " low ",
		"empty-value":    "",
		"off-attempt":    "off",
		"none-attempt":   "none",
		"unknown-tier-x": "turbo",
	})
	for _, tc := range []struct {
		name            string
		realm, model    string
		knownCapability bool
		wantValue       string
		wantOK          bool
	}{
		// 优先级：<realm>:<model> > <model> > <realm>:* > *
		{"域+模型最优先", "cn", "glm-5.2", true, "xhigh", true},
		{"裸模型次之", "global", "glm-5.2", true, "low", true},
		{"域通配", "cn", "whatever", true, "high", true},
		{"全局通配兜底", "global", "whatever", true, "medium", true},
		{"global 专属条目", "global", "hy3", true, "max", true},
		// 通配只对档位能力已知的模型生效（不拿通配给不认识的模型注入档位）。
		{"能力未知时通配不生效", "global", "whatever", false, "", false},
		// 显式逐模型条目不受能力已知与否影响（那是用户对具体模型的声明）。
		{"显式条目在能力未知时仍生效", "global", "glm-5.2", false, "low", true},
		{"空模型名不命中", "cn", "", true, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := EffortDefaultOverride(tc.realm, tc.model, tc.knownCapability)
			if ok != tc.wantOK || got != tc.wantValue {
				t.Fatalf("EffortDefaultOverride(%q,%q,known=%v)=(%q,%v) want (%q,%v)",
					tc.realm, tc.model, tc.knownCapability, got, ok, tc.wantValue, tc.wantOK)
			}
		})
	}
	// 空值条目被剔除（空 = 不覆盖）：命中时只能来自域通配 "cn:*"，不会是空串。
	if v, ok := EffortDefaultOverride("cn", "empty-value", true); !ok || v != "high" {
		t.Fatalf("空值条目应被剔除、由通配兜底: (%q,%v)", v, ok)
	}
	// 键与值都做大小写/空白归一（键面「glm-5.2」与「 spaced 」两种写法均命中）。
	if v, _ := EffortDefaultOverride("cn", " spaced ", true); v != "low" {
		t.Fatalf("键面未归一: %q", v)
	}
	// off/none 是「关闭思考」的开关值：不拒绝会静默打开/关掉思考，表里一律不存。
	// 单独用一张只有这两条的表格验证（否则域通配会兜底掩盖「是否真的被剔除」）。
	SetEffortDefaultOverrides(map[string]string{"off-attempt": "off", "none-attempt": "none"})
	for _, m := range []string{"off-attempt", "none-attempt"} {
		if v, ok := EffortDefaultOverride("cn", m, true); ok {
			t.Fatalf("%s 不应生效: %q", m, v)
		}
		if v, ok := defaultOverrideForModel("cn", m, []string{"low", "high"}); ok {
			t.Fatalf("关闭值不应被解析成默认档: %q", v)
		}
		if key, v := (&Client{}).RequestEffortDefault(map[string]any{}, "cn", m); key != "" || v != "" {
			t.Fatalf("关闭值不应被注入: key=%q effort=%q", key, v)
		}
	}
	// 未知档位名不是语法错误（档位集合由上游定义）：命中照传，由拒发阶段裁决。
	SetEffortDefaultOverrides(map[string]string{"unknown-tier-x": "turbo"})
	if v, ok := EffortDefaultOverride("cn", "unknown-tier-x", true); !ok || v != "turbo" {
		t.Fatalf("未知档位名应能命中（由拒发阶段裁决）: (%q,%v)", v, ok)
	}
}

// 覆盖值必须落到该模型的支持集上：宣告与执行一致，绝不出现「宣告了却不接受」的档位。
func TestEffortListingAppliesOverrideGroundedInSupported(t *testing.T) {
	resetEffortDefaultOverrides()
	t.Cleanup(resetEffortDefaultOverrides)

	// 基线（未配置）：remote 有档位就用 remote 默认档，静态兜底表用自身默认档。
	if _, def := EffortListing("cn", "glm-5.3-flash", nil, ""); def != "high" {
		t.Fatalf("基线默认档=%q", def)
	}
	SetEffortDefaultOverrides(map[string]string{"glm-5.3-flash": "low"})
	efforts, def := EffortListing("cn", "glm-5.3-flash", nil, "")
	if def != "low" {
		t.Fatalf("覆盖后默认档=%q want low（efforts=%v）", def, efforts)
	}
	// 远端探测桶权威不变：只有默认档被本地覆盖。
	efforts, def = EffortListing("cn", "glm-5.3-flash", []string{"high", "max", "low"}, "max")
	if def != "low" || len(efforts) != 3 {
		t.Fatalf("远端档位集被改动: %v/%q", efforts, def)
	}

	// 覆盖值不在支持集里 → 按档位序钳位（≤配置值的最高支持档 = low），而不是宣告越界值。
	SetEffortDefaultOverrides(map[string]string{"glm-5.3-flash": "max"})
	_, def = EffortListing("cn", "glm-5.3-flash", []string{"low", "medium"}, "medium")
	if def != "medium" {
		t.Fatalf("配置 max 落到 [low medium] 上应为 medium，实际 %q", def)
	}
	// 支持集全高于配置值 → 升到最低支持档（与 ClampEffort 同规则：比宣告越界值好）。
	SetEffortDefaultOverrides(map[string]string{"glm-5.3-flash": "low"})
	_, def = EffortListing("cn", "glm-5.3-flash", []string{"high", "xhigh"}, "")
	if def != "high" {
		t.Fatalf("配置 low 落到 [high xhigh] 上应为 high，实际 %q", def)
	}
	// 声明形态：上游大小写敏感，宣告也要回写它的写法。
	_, def = EffortListing("cn", "glm-5.3-flash", []string{"HIGH", "max"}, "")
	if def != "HIGH" {
		t.Fatalf("应按声明形态回写，实际 %q", def)
	}
	// 覆盖值非法/无法落地（支持集里只有关闭值）→ 回落原有默认档判定，不发越界值。
	SetEffortDefaultOverrides(map[string]string{"glm-5.3-flash": "off"})
	_, def = EffortListing("cn", "glm-5.3-flash", nil, "")
	if def != "high" {
		t.Fatalf("关闭值不应成为默认档，实际 %q", def)
	}
	SetEffortDefaultOverrides(map[string]string{"glm-5.3-flash": "low"})
	if _, def = EffortListing("cn", "glm-5.3-flash", []string{"off"}, ""); def != "" {
		t.Fatalf("支持集只有关闭值时不宣告默认档，实际 %q", def)
	}

	// 模型档位能力未知（两级都 miss）→ 仍然不宣告任何档位（覆盖表不负责编造能力）。
	SetEffortDefaultOverrides(map[string]string{"*": "low"})
	if efforts, def = EffortListing("cn", "no-such-model", nil, ""); efforts != nil || def != "" {
		t.Fatalf("未知模型被通配注入: %v/%q", efforts, def)
	}
}

// ClampEffortWithDefault：越界时的回退目标换成配置的默认档；未配置逐字回落旧规则；
// 请求档位本身受支持时一律不动（包括大小写形态归一——那不是越界）。
func TestClampEffortWithDefaultFallsBackToOverride(t *testing.T) {
	// cn/glm-5.2 静态档位集 = [high xhigh]。
	for _, tc := range []struct {
		name      string
		requested string
		supported []string
		preferred string
		want      string
		changed   bool
	}{
		// 越界：配置值优先于「≤请求档位的最高支持档」。
		{"越界落到本地默认档", "max", []string{"low", "medium", "high", "xhigh"}, "medium", "medium", true},
		{"越界且默认档=最低档", "max", []string{"low", "high", "xhigh"}, "low", "low", true},
		// 未配置 → 与 ClampEffort 逐字一致（降级优先、偏离最小）。
		{"未配置回落降级规则", "max", []string{"low", "high", "xhigh"}, "", "xhigh", true},
		{"未配置且全高于请求档", "low", []string{"high", "xhigh"}, "", "high", true},
		// 请求档位受支持：一律不动（本地默认档只在越界时兜底）。
		{"受支持不改写", "high", []string{"low", "high", "xhigh"}, "low", "", false},
		{"受支持但形态不同（不是越界）", "HIGH", []string{"low", "high"}, "low", "high", true},
		// 关闭思考值：永不被归一，也永不能当回退目标。
		{"off 不改写", "off", []string{"low", "high"}, "low", "", false},
		{"默认档为关闭值时忽略", "max", []string{"low", "high"}, "off", "high", true},
		// 默认档不在支持集里 → 回落旧规则（不 aid 发一个必 400 的值）。
		{"默认档不可用回落降级", "max", []string{"low", "medium"}, "xhigh", "medium", true},
		// 未知档位 / 能力未知：透传，绝不编造。
		{"请求档位未知不改写", "turbo", []string{"low", "high"}, "low", "", false},
		{"能力未知不改写", "max", nil, "low", "", false},
		// 支持集已证明新档位可执行，回退目标不要求出现在静态排序表里。
		{"新档位可作默认回退", "max", []string{"high", "Extreme"}, "extreme", "Extreme", true},
		{"支持集只有新档位", "max", []string{"Extreme"}, "extreme", "Extreme", true},
		{"默认是新档位但请求已支持", "HIGH", []string{"high", "Extreme"}, "extreme", "high", true},
		{"新默认档未被上游声明", "max", []string{"high"}, "extreme", "high", true},
		// 声明形态：回退目标按支持集的写法回写。
		{"默认档按声明形态回写", "max", []string{"Low", "High"}, "low", "Low", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := ClampEffortWithDefault(tc.requested, tc.supported, tc.preferred)
			if got != tc.want || changed != tc.changed {
				t.Fatalf("ClampEffortWithDefault(%q,%v,%q)=(%q,%v) want (%q,%v)",
					tc.requested, tc.supported, tc.preferred, got, changed, tc.want, tc.changed)
			}
			// 未配置覆盖时与公开 API ClampEffort 必须逐字同源（零行为变化）。
			if tc.preferred == "" {
				if g, c := ClampEffort(tc.requested, tc.supported); g != got || c != changed {
					t.Fatalf("未配置时与 ClampEffort 漂移: (%q,%v) vs (%q,%v)", g, c, got, changed)
				}
			}
		})
	}
}

func TestEffortDefaultNewTierConsistentAcrossPaths(t *testing.T) {
	resetEffortDefaultOverrides()
	t.Cleanup(resetEffortDefaultOverrides)
	SetEffortDefaultOverrides(map[string]string{"new-model": "extreme"})
	c := &Client{}
	c.storeEfforts("cn", map[string][]string{"new-model": {"high", "Extreme"}}, map[string]string{"new-model": "high"})
	_, listed := c.EffortSupport("cn", "new-model")
	_, injected := c.RequestEffortDefault(map[string]any{}, "cn", "new-model")
	_, _, fallback := c.RequestEffortClamp(map[string]any{"reasoning_effort": "max"}, "cn", "new-model")
	if listed != "Extreme" || injected != listed || fallback != listed {
		t.Fatalf("default drift: listed=%q injected=%q fallback=%q", listed, injected, fallback)
	}
}

// RequestEffortDefault：只在「客户端未表态 + 没有显式关思考 + 覆盖命中」时注入；
// 与 RequestEffortClamp 同为只读契约（写不写由调用方决定）。
func TestRequestEffortDefaultInjectsOnlyWhenClientSilent(t *testing.T) {
	resetEffortDefaultOverrides()
	t.Cleanup(resetEffortDefaultOverrides)
	// glm-5.3-flash 的 CN 静态档位集 = [low high max]：low 可落地，便于区分
	// 「逐模型条目」与「域通配」两条命中路径。
	SetEffortDefaultOverrides(map[string]string{"glm-5.3-flash": "low", "cn:*": "high"})
	c := &Client{}

	for _, tc := range []struct {
		name, obj, model, wantKey, wantVal string
	}{
		{"未指定档位 → 注入（逐模型条目优先）", `{"messages":[]}`, "glm-5.3-flash", "reasoning_effort", "low"},
		{"thinking.enabled → 注入", `{"thinking":{"type":"enabled"}}`, "glm-5.3-flash", "reasoning_effort", "low"},
		{"域通配命中其它已知模型", `{}`, "hy3", "reasoning_effort", "high"},
		// 客户端已表态（三种别名任一）→ 不注入：归一分支负责。
		{"snake 已指定", `{"reasoning_effort":"max"}`, "glm-5.3-flash", "", ""},
		{"camel 已指定", `{"reasoningEffort":"max"}`, "glm-5.3-flash", "", ""},
		{"reasoning.effort 已指定", `{"reasoning":{"effort":"high"}}`, "glm-5.3-flash", "", ""},
		// off/none 是「关掉思考」（客户端表态），不能被默认档打开。
		{"off 不注入", `{"reasoning_effort":"off"}`, "glm-5.3-flash", "", ""},
		// 显式关思考（thinking.type=disabled）不注入：注入会制造非法组合并打开思考。
		{"thinking.disabled 不注入", `{"thinking":{"type":"disabled"}}`, "glm-5.3-flash", "", ""},
		// 无档位能力（两级都 miss）→ 通配不生效，不替上游编造能力。
		{"能力未知不注入", `{}`, "no-such-model", "", ""},
		{"模型名为空", `{}`, "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := mustObject(t, tc.obj)
			key, val := c.RequestEffortDefault(obj, "cn", tc.model)
			if key != tc.wantKey || val != tc.wantVal {
				t.Fatalf("(%q,%q) want (%q,%q)", key, val, tc.wantKey, tc.wantVal)
			}
			if raw := mustJSON(t, obj); raw != tc.obj {
				t.Fatalf("mutated: %s want %s", raw, tc.obj)
			}
		})
	}
}

// 覆盖表是进程级快照，必须与 /v1/models 宣告、出站注入、越界回退三处同源；
// SetEffortDefaultOverrides(nil) 与空表必须把行为还原成「完全沿用上游」。
func TestSetEffortDefaultOverridesResetsBehavior(t *testing.T) {
	resetEffortDefaultOverrides()
	t.Cleanup(resetEffortDefaultOverrides)
	c := &Client{}

	SetEffortDefaultOverrides(map[string]string{"glm-5.3-flash": "low"})
	if _, def := c.EffortSupport("cn", "glm-5.3-flash"); def != "low" {
		t.Fatalf("覆盖未生效: %q", def)
	}
	for _, m := range []map[string]string{nil, {}} {
		SetEffortDefaultOverrides(m)
		if _, def := c.EffortSupport("cn", "glm-5.3-flash"); def != "high" {
			t.Fatalf("清空后应回落上游默认档 high，实际 %q", def)
		}
		if key, val := c.RequestEffortDefault(mustObject(t, `{}`), "cn", "glm-5.3-flash"); key != "" || val != "" {
			t.Fatalf("清空后不应注入: (%q,%q)", key, val)
		}
		if to, _ := ClampEffortWithDefault("max", []string{"high", "xhigh"}, ""); to != "xhigh" {
			t.Fatalf("清空后越界回退应回落旧规则，实际 %q", to)
		}
	}
	// 全空条目等价于空表（防「保存了空壳 map」把行为钉死）。
	SetEffortDefaultOverrides(map[string]string{"": "", "glm-5.3-flash": "  "})
	if _, def := c.EffortSupport("cn", "glm-5.3-flash"); def != "high" {
		t.Fatalf("空壳条目应等价空表，实际 %q", def)
	}
}
