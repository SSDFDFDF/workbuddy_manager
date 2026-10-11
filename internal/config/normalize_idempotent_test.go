package config

import (
	"encoding/json"
	"testing"
)

// TestNormalizeIdempotent normalize 必须幂等：Load 会走两次
// （parseObject 内一次 + applyEnv 之后再一次），保存路径也依赖它可以把
// 「已归一化的磁盘配置」再喂进 ParseConfigInto。不幂等会让 normalize 里新加的
// 归一/剪枝步骤在第二次调用时产生不同的配置（或反复删/补同一字段）。
func TestNormalizeIdempotent(t *testing.T) {
	raws := [][]byte{
		[]byte(`{"prompt":{"mode":"inject","preset":"official-craft","profiles":{"cn":{"mode":"","preset":"","text":"","file":""},"global":{"preset":"official-ask"}}}}`),
		[]byte(`{"prompt":{"mode":"after","profiles":{"cn":{"mode":"replace"}}}}`),
		[]byte(`{"schedule":{"checkin_hours":[9,9,8]},"pool":{"credit_floor":0},"media":{"image_max_dimension":1080}}`),
		[]byte(`{"growth":{"autotasks":{"disabled":[" a ","a",""],"only":[],"tasks":{"black_cat":{"window":"23:00-08:00"}}}}}`),
		[]byte(`{"listen":":1","session_sticky":{"enabled":true,"ttl":"45m"}}`),
		[]byte(`{"fingerprint_rewrite":true,"fingerprint_disabled":["err-11128","nope","err-11128"," "],"fingerprint_overrides":{"identity-claude-code":"X","hdr-billing":"Y","unknown":"Z","pr-branch":"  "},"fingerprint_rules":[{"match":"A","replace":"B"}]}`),
	}
	for i, raw := range raws {
		c, err := ParseConfig(raw) // ParseConfigInto 内含一次 normalize
		if err != nil {
			t.Fatalf("[%d] %v", i, err)
		}
		first, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.normalize(); err != nil {
			t.Fatalf("[%d] 第二次 normalize: %v", i, err)
		}
		second, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if string(first) != string(second) {
			t.Errorf("[%d] normalize 不幂等（第二次结果不同）:\n第一次: %s\n第二次: %s", i, first, second)
		}
	}
}
