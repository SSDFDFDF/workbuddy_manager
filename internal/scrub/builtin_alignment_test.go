package scrub

import "testing"

func TestBuiltinRulesAlignment(t *testing.T) {
	if len(literals)%2 != 0 {
		t.Fatalf("literals must be pairs, got %d", len(literals))
	}
	if len(literals)/2 != len(literalNotes) {
		t.Fatalf("literalNotes (%d) must match literal pairs (%d)", len(literalNotes), len(literals)/2)
	}
	rs := BuiltinRules()
	if len(rs) != len(literals)/2+3 {
		t.Fatalf("unexpected rule count %d", len(rs))
	}
	for _, r := range rs {
		if r.Match == "" {
			t.Fatalf("empty match in %+v", r)
		}
	}
}
