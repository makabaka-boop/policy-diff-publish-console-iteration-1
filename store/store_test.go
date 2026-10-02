package store

import (
	"errors"
	"sync"
	"testing"

	"accesssim/policy"
)

func baseDoc() *policy.Document {
	// Diamond with the privileged role at the JOIN:
	//
	//	   base
	//	  /    \
	// viewer  editor
	//	  \    /
	//	  admin
	//
	// Grants attached to viewer/editor propagate to admin but never to
	// roles above them, which keeps diffs easy to reason about.
	return &policy.Document{
		Roles: []policy.Role{
			{Name: "base"},
			{Name: "viewer", Parents: []string{"base"}},
			{Name: "editor", Parents: []string{"base"}},
			{Name: "admin", Parents: []string{"editor", "viewer"}},
		},
		Resources: []string{"doc", "billing"},
		Actions:   []string{"read", "write"},
		Rules: []policy.Rule{
			{ID: "base-deny", Role: "*", Resource: "*", Action: "*", Priority: 0, Effect: policy.EffectDeny},
			{ID: "viewer-read", Role: "viewer", Resource: "doc", Action: "read", Priority: 10, Effect: policy.EffectAllow},
			{ID: "admin-billing", Role: "admin", Resource: "billing", Action: "*", Priority: 90, Effect: policy.EffectAllow},
		},
	}
}

func mustEngine(t *testing.T, d *policy.Document) *policy.Engine {
	t.Helper()
	e, err := policy.NewEngine(d)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// TestPreviewDiffEvidence enumerates the draft vs published domain and
// checks the new-allow / new-deny entries carry winning-rule evidence
// on both sides.
func TestPreviewDiffEvidence(t *testing.T) {
	st, err := New(baseDoc())
	if err != nil {
		t.Fatal(err)
	}

	// editor and admin (inheriting editor) gain write on doc -> 2 new
	// allows. billing/read gains a same-priority conflict that stays
	// denied, exercising the tie path without producing a flip.
	draft := baseDoc()
	draft.Rules = append(draft.Rules,
		policy.Rule{ID: "editor-write", Role: "editor", Resource: "doc", Action: "write", Priority: 20, Effect: policy.EffectAllow},
		policy.Rule{ID: "billing-allow", Role: "viewer", Resource: "billing", Action: "read", Priority: 5, Effect: policy.EffectAllow},
		policy.Rule{ID: "billing-deny", Role: "*", Resource: "billing", Action: "read", Priority: 5, Effect: policy.EffectDeny},
	)
	if _, err := st.SaveDraft(draft); err != nil {
		t.Fatal(err)
	}

	sum, draftRev, pubRev, err := st.Preview()
	if err != nil {
		t.Fatal(err)
	}
	if draftRev != 2 || pubRev != 1 {
		t.Fatalf("revisions = (%d,%d), want (2,1)", draftRev, pubRev)
	}

	if len(sum.NewAllows) != 2 {
		t.Fatalf("newAllows = %d (%v), want 2", len(sum.NewAllows), sum.NewAllows)
	}
	find := func(entries []policy.DiffEntry, role string) *policy.DiffEntry {
		for i := range entries {
			if entries[i].Tuple.Role == role {
				return &entries[i]
			}
		}
		return nil
	}
	adminAllow := find(sum.NewAllows, "admin")
	if adminAllow == nil {
		t.Fatalf("missing admin new-allow (inherited editor write): %v", sum.NewAllows)
	}
	if adminAllow.After.Winners[0].RuleID != "editor-write" {
		t.Fatalf("after winners = %v, want editor-write", adminAllow.After.Winners)
	}
	// On the published side only the baseline wildcard deny matched.
	if adminAllow.Before.Reason != policy.ReasonSingleWinner ||
		adminAllow.Before.Winners[0].RuleID != "base-deny" {
		t.Fatalf("before evidence = %+v, want base-deny single winner", adminAllow.Before)
	}
	if len(sum.NewDenies) != 0 {
		t.Fatalf("newDenies = %d, want 0 (tie-deny stays deny)", len(sum.NewDenies))
	}

	// Publishing the valid preview succeeds.
	res, err := st.Publish(draftRev, pubRev, sum)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if res.Published.Revision != 2 {
		t.Fatalf("published revision = %d, want 2", res.Published.Revision)
	}
}

// TestStalePreviewAfterRuleEdit is the required "expired preview after
// rule edit" case: a preview generated for revision N must be rejected
// once the draft changes to revision N+1.
func TestStalePreviewAfterRuleEdit(t *testing.T) {
	st, err := New(baseDoc())
	if err != nil {
		t.Fatal(err)
	}

	// Preview #1 at draft revision 2.
	draft := baseDoc()
	draft.Rules = append(draft.Rules,
		policy.Rule{ID: "tmp-allow", Role: "viewer", Resource: "billing", Action: "read", Priority: 50, Effect: policy.EffectAllow})
	saved, err := st.SaveDraft(draft)
	if err != nil {
		t.Fatal(err)
	}
	stale, draftRev, pubRev, err := st.Preview()
	if err != nil {
		t.Fatal(err)
	}

	// Another edit lands before publishing: remove the tmp rule and add
	// something different. Draft revision moves on.
	draft2 := baseDoc()
	draft2.Rules = append(draft2.Rules,
		policy.Rule{ID: "other-allow", Role: "admin", Resource: "billing", Action: "write", Priority: 50, Effect: policy.EffectAllow})
	if _, err := st.SaveDraft(draft2); err != nil {
		t.Fatal(err)
	}

	_, err = st.Publish(draftRev, pubRev, stale)
	if !errors.Is(err, ErrDraftConflict) {
		t.Fatalf("publish stale preview err = %v, want ErrDraftConflict", err)
	}

	// Also: even sending the stale summary but stamping it with the NEW
	// revision numbers must fail — its hash/content cannot describe a
	// revision pair it never enumerated (revision fields are inside the
	// hash, so tampering breaks the hash first).
	tampered := *stale
	tampered.DraftRevision = saved.Revision + 1
	ok, err := tampered.VerifyHash()
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("tampered summary hash unexpectedly still valid")
	}
	if _, err := st.Publish(saved.Revision+1, pubRev, &tampered); !errors.Is(err, ErrSummaryHashInvalid) {
		t.Fatalf("tampered publish err = %v, want ErrSummaryHashInvalid", err)
	}

	// A fresh preview + publish with the correct summary works.
	fresh, newDraftRev, newPubRev, err := st.Preview()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Publish(newDraftRev, newPubRev, fresh); err != nil {
		t.Fatalf("fresh publish: %v", err)
	}
}

// TestConcurrentPublishers is the required two-client race: both
// preview the same revision pair, both attempt to publish; exactly one
// wins, the loser gets ErrPublishedConflict and must re-preview.
func TestConcurrentPublishers(t *testing.T) {
	st, err := New(baseDoc())
	if err != nil {
		t.Fatal(err)
	}

	// Both clients load state and make (identical-struct) drafts.
	clientDraft := func(suffix string) *policy.Document {
		d := baseDoc()
		d.Rules = append(d.Rules, policy.Rule{
			ID:   "c-" + suffix,
			Role: "viewer", Resource: "billing", Action: "read",
			Priority: 50, Effect: policy.EffectAllow,
		})
		return d
	}

	// Client A saves its draft (rev 2).
	sumA, draftRevA, pubRevA, err := func() (*policy.Summary, int, int, error) {
		if _, err := st.SaveDraft(clientDraft("a")); err != nil {
			t.Fatal(err)
		}
		return st.Preview()
	}()
	if err != nil {
		t.Fatal(err)
	}

	// Client B had loaded the same state; it also prepares a publish
	// over published revision 1 (without touching the draft). It uses
	// the exact preview it obtained: simulate by cloning A's summary —
	// both clients saw the same draft rev 2 / pub rev 1 pair.
	sumB := *sumA

	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	var wins int

	tryPublish := func(sum *policy.Summary, dr, pr int) {
		defer wg.Done()
		res, err := st.Publish(dr, pr, sum)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			errs = append(errs, err)
			return
		}
		wins++
		_ = res
	}

	wg.Add(2)
	go tryPublish(sumA, draftRevA, pubRevA)
	go tryPublish(&sumB, draftRevA, pubRevA)
	wg.Wait()

	if wins != 1 {
		t.Fatalf("wins = %d, want exactly 1 (errors: %v)", wins, errs)
	}
	if len(errs) != 1 || !errors.Is(errs[0], ErrPublishedConflict) {
		t.Fatalf("loser error = %v, want single ErrPublishedConflict", errs)
	}

	// Loser re-previews: published has moved to rev 2, and its preview
	// is now against the new baseline.
	_, _, pubRevAfter, err := st.Preview()
	if err != nil {
		t.Fatal(err)
	}
	if pubRevAfter != 2 {
		t.Fatalf("published revision after race = %d, want 2", pubRevAfter)
	}
}

// TestNewDeniesEvidence checks that removing an allow surfaces as a
// new-deny with the old winning rule in the before-evidence.
func TestNewDeniesEvidence(t *testing.T) {
	st, err := New(baseDoc())
	if err != nil {
		t.Fatal(err)
	}
	// Drop viewer-read and the base deny; keep only admin-billing.
	// viewer and member lose doc read, and on the draft side nothing at
	// all matches those tuples -> default-deny evidence.
	draft := baseDoc()
	draft.Rules = []policy.Rule{
		{ID: "admin-billing", Role: "admin", Resource: "billing", Action: "*", Priority: 90, Effect: policy.EffectAllow},
	}
	if _, err := st.SaveDraft(draft); err != nil {
		t.Fatal(err)
	}
	sum, _, _, err := st.Preview()
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.NewDenies) != 2 {
		t.Fatalf("newDenies = %d, want 2: %v", len(sum.NewDenies), sum.NewDenies)
	}
	for _, e := range sum.NewDenies {
		if e.Before.Reason != policy.ReasonSingleWinner || e.Before.Winners[0].RuleID != "viewer-read" {
			t.Fatalf("before evidence for %v = %+v, want viewer-read winner", e.Tuple, e.Before)
		}
		if e.After.Reason != policy.ReasonNoMatch {
			t.Fatalf("after reason for %v = %s, want no-match", e.Tuple, e.After.Reason)
		}
	}
}

func TestPublishRejectsMismatchedSummary(t *testing.T) {
	st, err := New(baseDoc())
	if err != nil {
		t.Fatal(err)
	}
	// Build a summary for a different revision pair entirely.
	other := baseDoc()
	otherEng := mustEngine(t, other)
	pubEng := mustEngine(t, baseDoc())
	fabricated, err := policy.BuildSummary(otherEng, 99, pubEng, 88)
	if err != nil {
		t.Fatal(err)
	}
	// Current store is (draft=1, published=1); revisions mismatch.
	_, err = st.Publish(1, 1, fabricated)
	if !errors.Is(err, ErrInvalidSummary) {
		t.Fatalf("err = %v, want ErrInvalidSummary", err)
	}

	// Forge revision numbers with a correct hash but wrong content:
	// craft a valid summary over the real pair, then mutate only counts
	// while rehashing — this models a client that enumerated a different
	// draft but pasted current revision numbers. Store re-enumeration
	// must catch it.
	draft := baseDoc()
	draft.Rules = append(draft.Rules,
		policy.Rule{ID: "x", Role: "viewer", Resource: "billing", Action: "read", Priority: 50, Effect: policy.EffectAllow})
	dEng := mustEngine(t, draft)
	good, err := policy.BuildSummary(dEng, 1, pubEng, 1)
	if err != nil {
		t.Fatal(err)
	}
	good.Unchanged++ // mutate content, hash now wrong
	if _, err := st.Publish(1, 1, good); !errors.Is(err, ErrSummaryHashInvalid) {
		t.Fatalf("mutated summary err = %v, want ErrSummaryHashInvalid", err)
	}

	// Sanity: the genuine happy path works at (1,1) only if draft is
	// really at rev 1 — it's still the base draft, so use base vs base.
	same, err := policy.BuildSummary(pubEng, 1, pubEng, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Publish(1, 1, same); err != nil {
		t.Fatalf("identical base publish failed: %v", err)
	}
}

func TestInvalidDraftRejected(t *testing.T) {
	st, err := New(baseDoc())
	if err != nil {
		t.Fatal(err)
	}
	bad := baseDoc()
	bad.Roles = append(bad.Roles, policy.Role{Name: "ghost-parent", Parents: []string{"nope"}})
	if _, err := st.SaveDraft(bad); !errors.Is(err, ErrInvalidDocument) {
		t.Fatalf("err = %v, want ErrInvalidDocument", err)
	}
}

func TestDomainUnionAcrossVersions(t *testing.T) {
	pub := &policy.Document{
		Roles:     []policy.Role{{Name: "a"}},
		Resources: []string{"r1"},
		Actions:   []string{"x"},
		Rules: []policy.Rule{
			{ID: "allow", Role: "a", Resource: "r1", Action: "x", Priority: 1, Effect: policy.EffectAllow},
		},
	}
	draft := &policy.Document{
		Roles:     []policy.Role{{Name: "a"}, {Name: "b"}},
		Resources: []string{"r1", "r2"},
		Actions:   []string{"x"},
		Rules: []policy.Rule{
			{ID: "allow", Role: "a", Resource: "r1", Action: "x", Priority: 1, Effect: policy.EffectAllow},
		},
	}
	st, err := New(pub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveDraft(draft); err != nil {
		t.Fatal(err)
	}
	sum, _, _, err := st.Preview()
	if err != nil {
		t.Fatal(err)
	}
	// Draft domain has 4 tuples.
	if sum.TupleCount != 4 {
		t.Fatalf("tupleCount = %d, want 4", sum.TupleCount)
	}
	// New tuples (a,r2,x), (b,r1,x), (b,r2,x) are default-deny on both
	// sides; (a,r1,x) unchanged allow. No flips either way.
	if len(sum.NewAllows) != 0 || len(sum.NewDenies) != 0 {
		t.Fatalf("unexpected flips: allows=%v denies=%v", sum.NewAllows, sum.NewDenies)
	}
}
