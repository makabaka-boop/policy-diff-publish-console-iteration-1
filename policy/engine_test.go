package policy

import "testing"

// diamondDoc builds:
//
//	admin
//	/   \
//	editor viewer
//	\   /
//	 member
func diamondDoc(rules ...Rule) *Document {
	return &Document{
		Roles: []Role{
			{Name: "member", Parents: []string{"editor", "viewer"}},
			{Name: "editor", Parents: []string{"admin"}},
			{Name: "viewer", Parents: []string{"admin"}},
			{Name: "admin"},
		},
		Resources: []string{"doc", "billing"},
		Actions:   []string{"read", "write", "delete"},
		Rules:     rules,
	}
}

// TestDiamondInheritance covers the required "role inheritance diamond"
// case: a rule attached to any ancestor node (including the top of the
// diamond, reached via two distinct paths) applies to the leaf role,
// exactly once per rule with no duplication effects.
func TestDiamondInheritance(t *testing.T) {
	doc := diamondDoc(
		Rule{ID: "admin-read", Role: "admin", Resource: "doc", Action: "read", Priority: 100, Effect: EffectAllow},
		Rule{ID: "viewer-billing", Role: "viewer", Resource: "billing", Action: "read", Priority: 100, Effect: EffectAllow},
		Rule{ID: "editor-write", Role: "editor", Resource: "doc", Action: "write", Priority: 100, Effect: EffectAllow},
	)
	eng, err := NewEngine(doc)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	cases := []struct {
		name             string
		tuple            Tuple
		want             string
		wantWinnerRuleID string
	}{
		{"member inherits admin through diamond (admin rule still matches once)",
			Tuple{"member", "doc", "read"}, EffectAllow, "admin-read"},
		{"member inherits viewer branch",
			Tuple{"member", "billing", "read"}, EffectAllow, "viewer-billing"},
		{"member inherits editor branch",
			Tuple{"member", "doc", "write"}, EffectAllow, "editor-write"},
		{"admin itself matches",
			Tuple{"admin", "doc", "read"}, EffectAllow, "admin-read"},
		{"viewer does NOT inherit editor's write",
			Tuple{"viewer", "doc", "write"}, EffectDeny, ""},
		{"unrelated action stays default deny",
			Tuple{"member", "doc", "delete"}, EffectDeny, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := eng.Decide(tc.tuple)
			if ev.Decision != tc.want {
				t.Fatalf("decision = %s, want %s (reason=%s)", ev.Decision, tc.want, ev.Reason)
			}
			if tc.wantWinnerRuleID == "" {
				if ev.Reason != ReasonNoMatch {
					t.Fatalf("expected no-match default deny, got reason=%s winners=%v", ev.Reason, ev.Winners)
				}
				return
			}
			if len(ev.Winners) != 1 || ev.Winners[0].RuleID != tc.wantWinnerRuleID {
				t.Fatalf("winners = %v, want single winner %s", ev.Winners, tc.wantWinnerRuleID)
			}
		})
	}

	// Diamond reachability must not double-count the single matching
	// rule even though admin is reachable member->editor->admin and
	// member->viewer->admin.
	ev := eng.Decide(Tuple{"member", "doc", "read"})
	count := 0
	for _, r := range ev.Considered {
		if r.RuleID == "admin-read" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("admin-read considered %d times, want exactly 1", count)
	}
}

// TestSamePriorityConflictDenyWins covers the required same-priority
// allow/deny conflict: deny wins, and both rules appear as evidence.
func TestSamePriorityConflictDenyWins(t *testing.T) {
	doc := diamondDoc(
		Rule{ID: "allow-billing", Role: "*", Resource: "billing", Action: "read", Priority: 20, Effect: EffectAllow},
		Rule{ID: "deny-billing", Role: "*", Resource: "billing", Action: "read", Priority: 20, Effect: EffectDeny},
	)
	eng, err := NewEngine(doc)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	ev := eng.Decide(Tuple{"member", "billing", "read"})
	if ev.Decision != EffectDeny {
		t.Fatalf("decision = %s, want deny", ev.Decision)
	}
	if ev.Reason != ReasonTieDeny {
		t.Fatalf("reason = %s, want %s", ev.Reason, ReasonTieDeny)
	}
	if len(ev.Winners) != 2 {
		t.Fatalf("winners = %v, want both conflicting rules", ev.Winners)
	}
	ids := map[string]bool{}
	for _, w := range ev.Winners {
		ids[w.RuleID] = true
	}
	if !ids["allow-billing"] || !ids["deny-billing"] {
		t.Fatalf("winners missing rules: %v", ids)
	}
}

// TestPriorityOverrides checks that only the highest matched priority
// group decides: a p50 allow overrides a p10 deny, while a p60 deny
// overrides that same allow.
func TestPriorityOverrides(t *testing.T) {
	base := diamondDoc(
		Rule{ID: "low-deny", Role: "*", Resource: "doc", Action: "read", Priority: 10, Effect: EffectDeny},
		Rule{ID: "high-allow", Role: "member", Resource: "doc", Action: "read", Priority: 50, Effect: EffectAllow},
	)
	eng, err := NewEngine(base)
	if err != nil {
		t.Fatal(err)
	}
	if ev := eng.Decide(Tuple{"member", "doc", "read"}); ev.Decision != EffectAllow || ev.Reason != ReasonSingleWinner {
		t.Fatalf("high allow should win: %+v", ev)
	}
	// viewer only matches the low deny
	if ev := eng.Decide(Tuple{"viewer", "doc", "read"}); ev.Decision != EffectDeny {
		t.Fatalf("viewer should be denied: %+v", ev)
	}

	base.Rules = append(base.Rules,
		Rule{ID: "higher-deny", Role: "*", Resource: "doc", Action: "read", Priority: 60, Effect: EffectDeny})
	eng, err = NewEngine(base)
	if err != nil {
		t.Fatal(err)
	}
	ev := eng.Decide(Tuple{"member", "doc", "read"})
	if ev.Decision != EffectDeny || ev.Reason != ReasonSingleWinner {
		t.Fatalf("higher deny should win: %+v", ev)
	}
	if ev.Winners[0].RuleID != "higher-deny" {
		t.Fatalf("winner = %v, want higher-deny", ev.Winners)
	}
}

func TestDefaultDenyWhenNoRuleMatches(t *testing.T) {
	eng, err := NewEngine(diamondDoc())
	if err != nil {
		t.Fatal(err)
	}
	ev := eng.Decide(Tuple{"admin", "doc", "delete"})
	if ev.Decision != EffectDeny || ev.Reason != ReasonNoMatch {
		t.Fatalf("got %+v, want no-match default deny", ev)
	}
}

func TestWildcards(t *testing.T) {
	doc := diamondDoc(
		Rule{ID: "wild", Role: "*", Resource: "*", Action: "*", Priority: 1, Effect: EffectAllow},
	)
	eng, err := NewEngine(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, tuple := range eng.Domain() {
		if ev := eng.Decide(tuple); ev.Decision != EffectAllow {
			t.Fatalf("tuple %v denied under wildcard allow: %+v", tuple, ev)
		}
	}
}

func TestDomainIsExhaustive(t *testing.T) {
	eng, err := NewEngine(diamondDoc())
	if err != nil {
		t.Fatal(err)
	}
	d := eng.Domain()
	if len(d) != 4*2*3 {
		t.Fatalf("domain size = %d, want %d", len(d), 4*2*3)
	}
}

func TestCycleRejected(t *testing.T) {
	doc := &Document{
		Roles: []Role{
			{Name: "a", Parents: []string{"b"}},
			{Name: "b", Parents: []string{"a"}},
		},
		Resources: []string{"r"},
		Actions:   []string{"x"},
	}
	if _, err := NewEngine(doc); err == nil {
		t.Fatal("expected cycle validation error")
	}

	// Self loop.
	doc2 := &Document{
		Roles:     []Role{{Name: "solo", Parents: []string{"solo"}}},
		Resources: []string{"r"},
		Actions:   []string{"x"},
	}
	if _, err := NewEngine(doc2); err == nil {
		t.Fatal("expected self-parent validation error")
	}
}

func TestCapsAndUnknownRefs(t *testing.T) {
	doc := &Document{
		Roles:     []Role{{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"}, {Name: "e"}, {Name: "f"}, {Name: "g"}, {Name: "h"}, {Name: "i"}},
		Resources: []string{"r"},
		Actions:   []string{"x"},
	}
	if err := doc.Validate(); err == nil {
		t.Fatal("expected >8 roles rejection")
	}

	bad := diamondDoc(Rule{ID: "ghost", Role: "ghost", Resource: "doc", Action: "read", Priority: 1, Effect: EffectAllow})
	if err := bad.Validate(); err == nil {
		t.Fatal("expected unknown role reference rejection")
	}
}
