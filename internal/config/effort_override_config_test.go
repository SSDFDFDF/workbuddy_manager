package config

import (
	"reflect"
	"strings"
	"testing"
)

// upstream.effort_default_overrides（模型默认思考档的本地覆盖）的归一化与校验：
// 键位/取值归一、空值剔除、未知域前缀告警剔除、关闭思考值告警剔除、
// 形态非法 fail fast（拼错要立刻知道）、清空后回落 nil（落盘形状稳定）。
func TestEffortDefaultOverridesNormalize(t *testing.T) {
	raw := []byte(`{
		"upstream": {
			"effort_default_overrides": {
				"GLM-5.2": "XHigh",
				"cn:deepseek-v4.1-flash": "low",
				"global:deepseek-v4.1-flash": "high",
				"*": "medium",
				"cn:*": "high",
				"  spaced  ": " low ",
				"empty-value": "   ",
				"off-attempt": "off",
				"none-attempt": "none",
				"badrealm:model": "low",
				"cn:": "low",
				"turbo-model": "turbo-tier"
			}
		}
	}`)
	cfg, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	got := cfg.Upstream.EffortDefaultOverrides
	want := map[string]string{
		"glm-5.2":                    "xhigh",
		"cn:deepseek-v4.1-flash":     "low",
		"global:deepseek-v4.1-flash": "high",
		"*":                          "medium",
		"cn:*":                       "high",
		"spaced":                     "low",
		"turbo-model":                "turbo-tier",
	}
	if len(got) != len(want) {
		t.Fatalf("条目数=%d want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("[%s]=%q want %q", k, got[k], v)
		}
	}
	warns := strings.Join(cfg.Warnings, "\n")
	for _, frag := range []string{"未知域前缀 badrealm", "只有域前缀没有模型名", "off 是关闭思考的开关值", "none 是关闭思考的开关值", "不在已知档位表内"} {
		if !strings.Contains(warns, frag) {
			t.Errorf("缺少告警 %q：%s", frag, warns)
		}
	}
	if strings.Contains(warns, "empty-value") {
		t.Errorf("空值条目应静默剔除（面板清空某行的语义），不该告警: %s", warns)
	}

	// 形态非法（满足不了标识符式）→ fail fast，不静默回落。
	if _, err := ParseConfig([]byte(`{"upstream":{"effort_default_overrides":{"glm-5.2":"极高"}}}`)); err == nil {
		t.Fatal("非法档位名形态应报错")
	}
	if _, err := ParseConfig([]byte(`{"upstream":{"effort_default_overrides":{"glm-5.2":"a b"}}}`)); err == nil {
		t.Fatal("含空格的档位名应报错")
	}

	// 清空（空对象 / 全空值）→ 字段回落 nil，落盘不出现空壳 map。
	for _, raw := range []string{
		`{"upstream":{"effort_default_overrides":{}}}`,
		`{"upstream":{"effort_default_overrides":{"glm-5.2":"  ","":"low"}}}`,
		`{"upstream":{"effort_default_overrides":null}}`,
	} {
		c, err := ParseConfig([]byte(raw))
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if c.Upstream.EffortDefaultOverrides != nil {
			t.Errorf("%s 应回落 nil，实际 %v", raw, c.Upstream.EffortDefaultOverrides)
		}
	}
}

// 未配置该字段时不得凭空产生条目（老配置零行为变化），且 §schema 的 omitempty
// 保证落盘里不出现该键。
func TestEffortDefaultOverridesAbsentByDefault(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"listen":":1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Upstream.EffortDefaultOverrides != nil {
		t.Fatalf("缺省不该有覆盖表: %v", cfg.Upstream.EffortDefaultOverrides)
	}
	if d := Default(); d.Upstream.EffortDefaultOverrides != nil {
		t.Fatalf("Default() 不该有覆盖表: %v", d.Upstream.EffortDefaultOverrides)
	}
}

// 显式提交覆盖表时整体替换；不提交则保留，所有解析入口使用同一规则。
func TestEffortDefaultOverridesReplaceOnParse(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want map[string]string
	}{
		{"大小写更新且删除旧行", `{"upstream":{"effort_default_overrides":{"GLM-5.2":"xhigh"}}}`, map[string]string{"glm-5.2": "xhigh"}},
		{"空表清空", `{"upstream":{"effort_default_overrides":{}}}`, nil},
		{"null 清空", `{"upstream":{"effort_default_overrides":null}}`, nil},
		{"空值清空", `{"upstream":{"effort_default_overrides":{"glm-5.2":""}}}`, nil},
		{"未提交保留", `{"upstream":{"tail_seconds":10}}`, map[string]string{"glm-5.2": "high", "hy3": "low"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i := 0; i < 32; i++ {
				base, err := ParseConfig([]byte(`{"upstream":{"effort_default_overrides":{"glm-5.2":"high","hy3":"low"}}}`))
				if err != nil {
					t.Fatal(err)
				}
				got, err := ParseConfigInto([]byte(tc.raw), base)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got.Upstream.EffortDefaultOverrides, tc.want) {
					t.Fatalf("overrides=%v want %v", got.Upstream.EffortDefaultOverrides, tc.want)
				}
			}
		})
	}
}

func TestEffortDefaultOverridesRejectNormalizedDuplicateKeys(t *testing.T) {
	for _, raw := range []string{
		`{"upstream":{"effort_default_overrides":{"glm-5.2":"high","GLM-5.2":"xhigh"}}}`,
		`{"upstream":{"effort_default_overrides":{"cn:glm-5.2":"high"," CN:GLM-5.2 ":"xhigh"}}}`,
		`{"upstream":{"effort_default_overrides":{"*":"low"," * ":"high"}}}`,
		`{"upstream":{"effort_default_overrides":{"glm-5.2":""," GLM-5.2 ":"high"}}}`,
	} {
		for i := 0; i < 16; i++ {
			if _, err := ParseConfig([]byte(raw)); err == nil || !strings.Contains(err.Error(), "归一后重复") {
				t.Fatalf("ambiguous keys must fail: raw=%s err=%v", raw, err)
			}
		}
	}
}
