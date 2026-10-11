package scrub

import "testing"

func TestBuiltinRulesAlignment(t *testing.T) {
	if len(literals)%2 != 0 {
		t.Fatalf("literals must be pairs, got %d", len(literals))
	}
	if len(literals)/2 != len(literalNotes) {
		t.Fatalf("literalNotes (%d) must match literal pairs (%d)", len(literalNotes), len(literals)/2)
	}
	if len(literalIDs) != len(literals)/2 {
		t.Fatalf("literalIDs (%d) must match literal pairs (%d)", len(literalIDs), len(literals)/2)
	}
	rs := BuiltinRules()
	if len(rs) != len(literals)/2+3 {
		t.Fatalf("unexpected rule count %d", len(rs))
	}
	ids := make(map[string]bool)
	for _, r := range rs {
		if r.ID == "" {
			t.Fatalf("empty ID in %+v", r)
		}
		if ids[r.ID] {
			t.Fatalf("duplicate ID %q in BuiltinRules", r.ID)
		}
		ids[r.ID] = true
		if !IsValidBuiltinID(r.ID) {
			t.Fatalf("IsValidBuiltinID(%q) should be true", r.ID)
		}
		if r.Match == "" {
			t.Fatalf("empty match in %+v", r)
		}
		// 有替换文本 ⇔ 支持覆盖（delete-only 模式不可覆盖，配置侧会告警剔除）。
		if got, want := ReplaceableBuiltin(r.ID), r.Replace != ""; got != want {
			t.Fatalf("ReplaceableBuiltin(%q)=%v want %v", r.ID, got, want)
		}
	}
	if ReplaceableBuiltin("no-such-rule") {
		t.Fatal("未知 ID 不应可覆盖")
	}
}
