package store

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"accesssim/policy"
)

// fakeClock is a manually advanced time source for deterministic expiry
// boundary testing.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }
func (c *fakeClock) advance(d time.Duration) {
	c.t = c.t.Add(d)
}

func newStoreWithFakeClock(t *testing.T) (*Store, *fakeClock) {
	t.Helper()
	clk := &fakeClock{t: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)}
	st, err := NewWithClock(baseDoc(), clk.now)
	if err != nil {
		t.Fatal(err)
	}
	return st, clk
}

// deniedTuple is a tuple that is denied by baseDoc(): "base" inherits
// nothing, only the wildcard deny p0 matches (billing, read).
var deniedTuple = policy.Tuple{Role: "base", Resource: "billing", Action: "read"}

// allowedTuple is allowed by baseDoc(): viewer-read p10 allow.
var allowedTuple = policy.Tuple{Role: "viewer", Resource: "doc", Action: "read"}

func reqFor(t policy.Tuple, ttl int) ExceptionRequest {
	return ExceptionRequest{
		Role: t.Role, Resource: t.Resource, Action: t.Action,
		Reason: "drill: instructor-led emergency override", TTLMinutes: ttl,
	}
}

// TestExceptionCreateAndPublishedOverride covers the happy path: the
// published decision flips to allow carrying the preserved base deny
// evidence and the exception identity, while the draft decision and the
// preview diff never see the override.
func TestExceptionCreateAndPublishedOverride(t *testing.T) {
	st, clk := newStoreWithFakeClock(t)

	view, eff, err := st.CreateException(reqFor(deniedTuple, 10))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if view.ID == "" {
		t.Fatal("exception has no id")
	}
	wantExpires := clk.t.Add(10 * time.Minute)
	if !view.ExpiresAt.Equal(wantExpires) {
		t.Fatalf("expires = %v, want %v", view.ExpiresAt, wantExpires)
	}
	if view.PublishedRevision != 1 {
		t.Fatalf("pinned revision = %d, want 1", view.PublishedRevision)
	}

	// Effective evidence: allow, with BOTH the exception identity and the
	// untouched base ruling — never a bare rule-deny next to an allow.
	if eff.Decision != policy.EffectAllow {
		t.Fatalf("effective decision = %s, want allow", eff.Decision)
	}
	if eff.Reason != policy.ReasonEmergencyExcept {
		t.Fatalf("reason = %s, want %s", eff.Reason, policy.ReasonEmergencyExcept)
	}
	if eff.Exception == nil || eff.Exception.ID != view.ID {
		t.Fatalf("exception ref = %+v, want id %s", eff.Exception, view.ID)
	}
	if eff.BaseEvidence == nil {
		t.Fatal("missing base evidence: override must preserve the original ruling")
	}
	if eff.BaseEvidence.Decision != policy.EffectDeny {
		t.Fatalf("base decision = %s, want deny", eff.BaseEvidence.Decision)
	}
	if len(eff.BaseEvidence.Winners) != 1 || eff.BaseEvidence.Winners[0].RuleID != "base-deny" {
		t.Fatalf("base winners = %+v, want base-deny", eff.BaseEvidence.Winners)
	}
	// The original rule chain stays visible on the override row.
	if len(eff.Winners) != 1 || eff.Winners[0].RuleID != "base-deny" {
		t.Fatalf("override winners = %+v, want the underlying base-deny rule", eff.Winners)
	}

	// Bulk published decisions show the same coherent override.
	_, rows, err := st.Decisions("published")
	if err != nil {
		t.Fatal(err)
	}
	var found *DecisionRow
	for i := range rows {
		if rows[i].Tuple == deniedTuple {
			found = &rows[i]
		}
	}
	if found == nil {
		t.Fatal("denied tuple missing from published domain")
	}
	if found.Evidence.Decision != policy.EffectAllow ||
		found.Evidence.Exception == nil || found.Evidence.BaseEvidence == nil {
		t.Fatalf("bulk row incoherent: %+v", found.Evidence)
	}
	if found.Evidence.Exception.ID != view.ID {
		t.Fatalf("bulk row exception id = %s, want %s", found.Evidence.Exception.ID, view.ID)
	}

	// Draft decisions are computed from draft rules only: still deny, no
	// exception data anywhere.
	_, drows, err := st.Decisions("draft")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range drows {
		if r.Evidence.Exception != nil || r.Evidence.BaseEvidence != nil {
			t.Fatalf("draft row carries exception data: %+v", r)
		}
		if r.Tuple == deniedTuple && r.Evidence.Decision != policy.EffectDeny {
			t.Fatalf("draft decision for tuple = %s, exceptions must not touch the draft", r.Evidence.Decision)
		}
	}

	// Preview compares pure policy on both sides: the tuple is deny->deny
	// and must not appear as a new allow.
	sum, _, _, err := st.Preview()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range sum.NewAllows {
		if e.Tuple == deniedTuple {
			t.Fatal("preview newAllows includes the emergency-exception tuple; previews must be computed on pure policy")
		}
	}
}

// TestExceptionTTLBoundaries asserts the 1..60 minute window and the
// exclusive expiry instant (active at expires-1ns, gone at expires), with
// recovery to the original ruling requiring no background task.
func TestExceptionTTLBoundaries(t *testing.T) {
	st, clk := newStoreWithFakeClock(t)

	for _, ttl := range []int{0, -5, 61, 100000} {
		if _, _, err := st.CreateException(reqFor(deniedTuple, ttl)); !errors.Is(err, ErrInvalidExceptionRequest) {
			t.Fatalf("ttl=%d err = %v, want ErrInvalidExceptionRequest", ttl, err)
		}
	}

	// A rejected create must leave NOTHING behind ("整次拒绝").
	if rev, list, err := st.ListExceptions(); err != nil || len(list) != 0 || rev != 1 {
		t.Fatalf("after invalid TTL: rev=%d list=%v err=%v, want empty", rev, list, err)
	}

	view, _, err := st.CreateException(reqFor(deniedTuple, 1))
	if err != nil {
		t.Fatal(err)
	}

	// One nanosecond before expiry: still active.
	clk.advance(time.Minute - time.Nanosecond)
	assertPublished(t, st, deniedTuple, policy.EffectAllow, view.ID)
	if rev, list, err := st.ListExceptions(); err != nil || len(list) != 1 || rev != 1 {
		t.Fatalf("pre-expiry list rev=%d len=%d err=%v, want 1", rev, len(list), err)
	}

	// Exactly the expiry instant: expired (expiry is exclusive). The
	// published ruling is back to deny, again without any reaper task.
	clk.advance(time.Nanosecond)
	assertPublished(t, st, deniedTuple, policy.EffectDeny, "")
	rev, list, err := st.ListExceptions()
	if err != nil || len(list) != 0 || rev != 1 {
		t.Fatalf("post-expiry list rev=%d len=%d err=%v, want empty at same revision", rev, len(list), err)
	}

	// After expiry the same tuple can be excepted again.
	view2, _, err := st.CreateException(reqFor(deniedTuple, 60))
	if err != nil {
		t.Fatalf("re-create after expiry: %v", err)
	}
	if view2.ID == view.ID {
		t.Fatal("re-created exception reused the old id")
	}
	clk.advance(59*time.Minute + 59*time.Second + 999999999)
	assertPublished(t, st, deniedTuple, policy.EffectAllow, view2.ID)
	clk.advance(time.Nanosecond)
	assertPublished(t, st, deniedTuple, policy.EffectDeny, "")
}

// TestExceptionInvalidRequestValidation covers reason bounds and
// malformed/wildcard tuples: every such request is rejected wholesale.
func TestExceptionInvalidRequestValidation(t *testing.T) {
	st, _ := newStoreWithFakeClock(t)

	long := make([]byte, MaxExceptionReason+1)
	for i := range long {
		long[i] = 'x'
	}
	bad := []ExceptionRequest{
		{Role: "base", Resource: "billing", Action: "read", TTLMinutes: 5, Reason: "   "},
		{Role: "base", Resource: "billing", Action: "read", TTLMinutes: 5, Reason: string(long)},
		{Role: "", Resource: "billing", Action: "read", TTLMinutes: 5, Reason: "x"},
		{Role: "*", Resource: "billing", Action: "read", TTLMinutes: 5, Reason: "x"},
		{Role: "base", Resource: "*", Action: "read", TTLMinutes: 5, Reason: "x"},
		{Role: "base", Resource: "billing", Action: "*", TTLMinutes: 5, Reason: "x"},
	}
	for i, r := range bad {
		if _, _, err := st.CreateException(r); !errors.Is(err, ErrInvalidExceptionRequest) {
			t.Fatalf("case #%d err = %v, want ErrInvalidExceptionRequest", i, err)
		}
	}
	if _, list, err := st.ListExceptions(); err != nil || len(list) != 0 {
		t.Fatalf("list after rejected requests = %v (err %v)", list, err)
	}
}

// TestExceptionTupleMustBeConcretePublishedDomainAndDenied: a tuple
// outside the published finite domain, or one the published policy
// already allows, cannot establish an exception.
func TestExceptionTupleMustBeConcretePublishedDomainAndDenied(t *testing.T) {
	st, _ := newStoreWithFakeClock(t)

	// Unknown role/resource/action -> outside the published domain.
	outside := []policy.Tuple{
		{Role: "ghost", Resource: "billing", Action: "read"},
		{Role: "base", Resource: "nowhere", Action: "read"},
		{Role: "base", Resource: "billing", Action: "explode"},
	}
	for _, tpl := range outside {
		if _, _, err := st.CreateException(reqFor(tpl, 5)); !errors.Is(err, ErrTupleOutsideDomain) {
			t.Fatalf("%v err = %v, want ErrTupleOutsideDomain", tpl, err)
		}
	}

	// Currently allowed tuple -> whole request rejected.
	if _, ev, err := st.CreateException(reqFor(allowedTuple, 5)); !errors.Is(err, ErrTupleNotDenied) {
		t.Fatalf("allowed tuple err = %v (ev %+v), want ErrTupleNotDenied", err, ev)
	}
	// The rejected allow-tuple must not have sprouted an exception.
	assertPublished(t, st, allowedTuple, policy.EffectAllow, "")
	if _, list, _ := st.ListExceptions(); len(list) != 0 {
		t.Fatalf("exceptions after rejections: %v, want none", list)
	}
}

// TestExceptionDuplicateTuple: a second active exception for the same
// exact tuple is rejected while the first stays in force.
func TestExceptionDuplicateTuple(t *testing.T) {
	st, clk := newStoreWithFakeClock(t)

	first, _, err := st.CreateException(reqFor(deniedTuple, 10))
	if err != nil {
		t.Fatal(err)
	}
	_, eff, err := st.CreateException(reqFor(deniedTuple, 20))
	if !errors.Is(err, ErrExceptionAlreadyActive) {
		t.Fatalf("duplicate err = %v, want ErrExceptionAlreadyActive", err)
	}
	// The conflicting response describes the EXISTING override.
	if eff.Exception == nil || eff.Exception.ID != first.ID {
		t.Fatalf("conflict evidence = %+v, want existing exception %s", eff.Exception, first.ID)
	}
	clk.advance(11 * time.Minute)
	if _, list, _ := st.ListExceptions(); len(list) != 0 {
		t.Fatalf("list = %v, first exception should have expired", list)
	}
}

// TestExceptionPublishInvalidatesAndLateCreateCannotAttach is the
// required publish-race story, driven deterministically by interleaving
// the requests on one goroutine:
//
//  1. create an exception on published revision p1;
//  2. publish a new policy -> p2, which immediately clears the exception;
//  3. the "late" create that was adjudicated against p1 cannot attach to
//     p2: create re-adjudicates against the current published revision,
//     so it succeeds ONLY if p2 really denies the tuple, and is pinned
//     to p2 when it does;
//  4. when p2 allows the tuple, the late request is rejected instead of
//     silently carrying an old-p1 override onto p2.
func TestExceptionPublishInvalidatesAndLateCreateCannotAttach(t *testing.T) {
	t.Run("new revision still denies: late create re-adjudicates against new revision", func(t *testing.T) {
		st, clk := newStoreWithFakeClock(t)
		old, _, err := st.CreateException(reqFor(deniedTuple, 30))
		if err != nil {
			t.Fatal(err)
		}

		// Publish a different draft: keep (base, billing, read) denied,
		// change another rule so the published content really changes.
		draft := baseDoc()
		draft.Rules = append(draft.Rules, policy.Rule{
			ID: "extra", Role: "viewer", Resource: "doc", Action: "write",
			Priority: 20, Effect: policy.EffectAllow,
		})
		newRev := publishDraft(t, st, draft)
		if newRev != 2 {
			t.Fatalf("new revision = %d, want 2", newRev)
		}

		// The old p1 exception is gone immediately, well before 30 minutes.
		assertPublished(t, st, deniedTuple, policy.EffectDeny, "")
		if _, list, _ := st.ListExceptions(); len(list) != 0 {
			t.Fatalf("exceptions after publish: %+v, want cleared", list)
		}

		// A late-arriving create cannot reuse the old exception; it is
		// re-adjudicated under p2 and, since p2 still denies, attaches
		// only to p2.
		late, eff, err := st.CreateException(reqFor(deniedTuple, 5))
		if err != nil {
			t.Fatalf("late create against denying p2: %v", err)
		}
		if late.ID == old.ID {
			t.Fatal("late exception reused the p1 exception id")
		}
		if late.PublishedRevision != 2 {
			t.Fatalf("late exception pinned to %d, want 2", late.PublishedRevision)
		}
		if eff.Exception.PublishedRevision != 2 {
			t.Fatalf("effective evidence pinned to %d, want 2", eff.Exception.PublishedRevision)
		}
		// It expires against its own 5-minute TTL, never silently
		// resurrecting the cleared p1 exception.
		clk.advance(6 * time.Minute)
		assertPublished(t, st, deniedTuple, policy.EffectDeny, "")
	})

	t.Run("new revision allows: late create is rejected, no attachment", func(t *testing.T) {
		st, clk := newStoreWithFakeClock(t)
		if _, _, err := st.CreateException(reqFor(deniedTuple, 30)); err != nil {
			t.Fatal(err)
		}
		// p2 explicitly allows (base, billing, read).
		draft := baseDoc()
		draft.Rules = append(draft.Rules, policy.Rule{
			ID: "allow-base-billing-read", Role: "base", Resource: "billing", Action: "read",
			Priority: 40, Effect: policy.EffectAllow,
		})
		if rev := publishDraft(t, st, draft); rev != 2 {
			t.Fatalf("new revision = %d, want 2", rev)
		}
		// p2 ruling is allow; no stale p1 override survives.
		assertPublished(t, st, deniedTuple, policy.EffectAllow, "")

		_, _, err := st.CreateException(reqFor(deniedTuple, 30))
		if !errors.Is(err, ErrTupleNotDenied) {
			t.Fatalf("late create err = %v, want ErrTupleNotDenied", err)
		}
		if _, list, _ := st.ListExceptions(); len(list) != 0 {
			t.Fatalf("exceptions = %v, want none", list)
		}
		clk.advance(time.Minute)
		assertPublished(t, st, deniedTuple, policy.EffectAllow, "")
	})
}

// TestExceptionConcurrentCreateAndPublish stresses the shared-version
// adjudication: under heavy interleaving every stored exception must be
// pinned to the then-current published revision, and that revision's pure
// ruling for the tuple must be deny. It also checks no ids collide.
func TestExceptionConcurrentCreateAndPublish(t *testing.T) {
	st, clk := newStoreWithFakeClock(t)

	const workers = 16
	const iterations = 50
	var wg sync.WaitGroup
	var createMu sync.Mutex
	var createErrs []error

	// One group hammers exception creation for the always-denied tuple.
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				_, _, err := st.CreateException(reqFor(deniedTuple, 1))
				if err != nil &&
					!errors.Is(err, ErrExceptionAlreadyActive) &&
					!errors.Is(err, ErrTupleNotDenied) {
					createMu.Lock()
					createErrs = append(createErrs, err)
					createMu.Unlock()
				}
			}
		}()
	}

	// Other goroutines publish (which clears exceptions) and list.
	var publishCount int64
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				draft := baseDoc()
				draft.Rules = append(draft.Rules, policy.Rule{
					ID: fmt.Sprintf("pub-%d-%d", w, i), Role: "viewer", Resource: "doc", Action: "write",
					Priority: 20 + i, Effect: policy.EffectAllow,
				})
				if _, err := st.SaveDraft(draft); err != nil {
					t.Errorf("save draft: %v", err)
					return
				}
				sum, dr, pr, err := st.Preview()
				if err != nil {
					t.Errorf("preview: %v", err)
					return
				}
				if _, err := st.Publish(dr, pr, sum); err == nil {
					atomic.AddInt64(&publishCount, 1)
				}
			}
		}(w)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			rev, list, err := st.ListExceptions()
			if err != nil {
				t.Errorf("list: %v", err)
				return
			}
			_ = rev
			_ = list
		}
	}()
	wg.Wait()

	if len(createErrs) != 0 {
		t.Fatalf("unexpected create errors: %v", createErrs[:min(5, len(createErrs))])
	}
	if atomic.LoadInt64(&publishCount) == 0 {
		t.Fatal("no publish succeeded in the interleaving run")
	}

	// Global invariant after the storm: re-evaluate under the clock that
	// has not advanced, then verify every listed exception is coherent.
	snap, err := st.publishedSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	rev, list, err := st.ListExceptions()
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, v := range list {
		if v.PublishedRevision != rev {
			t.Fatalf("exception %s pinned to %d but current published is %d", v.ID, v.PublishedRevision, rev)
		}
		if v.ID == "" || ids[v.ID] {
			t.Fatalf("duplicate or empty exception id %q", v.ID)
		}
		ids[v.ID] = true
		// The current published policy must really deny the tuple.
		if base := snap.eng.Decide(v.Tuple); base.Decision != policy.EffectDeny {
			t.Fatalf("exception %s covers tuple %v that is %s under its pinned policy", v.ID, v.Tuple, base.Decision)
		}
		// Effective row must be a coherent allow+baseEvidence+exception.
		eff := snap.decide(v.Tuple)
		if eff.Decision != policy.EffectAllow || eff.Exception == nil || eff.BaseEvidence == nil {
			t.Fatalf("incoherent override row for %s: %+v", v.ID, eff)
		}
		if eff.Exception.ID != v.ID {
			t.Fatalf("row identity = %s, want %s", eff.Exception.ID, v.ID)
		}
	}

	// Expiry reaper still works without any background task.
	clk.advance(2 * time.Minute)
	if _, list, _ := st.ListExceptions(); len(list) != 0 {
		t.Fatalf("list after ttl elapsed = %v, want empty", list)
	}
}

// TestExceptionResetClearsEverything verifies the demo-reset swap drops
// exceptions even when the TTL has not elapsed.
func TestExceptionResetClearsEverything(t *testing.T) {
	st, clk := newStoreWithFakeClock(t)
	if _, _, err := st.CreateException(reqFor(deniedTuple, 60)); err != nil {
		t.Fatal(err)
	}
	st2, err := newStore(baseDoc(), clk.now)
	if err != nil {
		t.Fatal(err)
	}
	st.Replace(st2) // mirrors the API handler's atomic swap onto fresh data
	if _, list, err := st.ListExceptions(); err != nil || len(list) != 0 {
		t.Fatalf("after reset list = %v err = %v, want empty", list, err)
	}
	assertPublished(t, st, deniedTuple, policy.EffectDeny, "")
}

// ---- helpers ----

func assertPublished(t *testing.T, st *Store, tpl policy.Tuple, wantDecision, wantExceptionID string) {
	t.Helper()
	snap, err := st.publishedSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	ev := snap.decide(tpl)
	if ev.Decision != wantDecision {
		t.Fatalf("decision for %v = %s, want %s (evidence %+v)", tpl, ev.Decision, wantDecision, ev)
	}
	if wantExceptionID == "" {
		if ev.Exception != nil {
			t.Fatalf("decision for %v unexpectedly carries exception %s", tpl, ev.Exception.ID)
		}
		if ev.BaseEvidence != nil {
			t.Fatalf("decision for %v unexpectedly carries base evidence", tpl)
		}
	} else {
		if ev.Exception == nil || ev.Exception.ID != wantExceptionID {
			got := ""
			if ev.Exception != nil {
				got = ev.Exception.ID
			}
			t.Fatalf("decision for %v exception = %q, want %q", tpl, got, wantExceptionID)
		}
		if ev.BaseEvidence == nil || ev.BaseEvidence.Decision != policy.EffectDeny {
			t.Fatalf("decision for %v missing deny base evidence: %+v", tpl, ev.BaseEvidence)
		}
	}
}

func publishDraft(t *testing.T, st *Store, draft *policy.Document) int {
	t.Helper()
	if _, err := st.SaveDraft(draft); err != nil {
		t.Fatal(err)
	}
	sum, dr, pr, err := st.Preview()
	if err != nil {
		t.Fatal(err)
	}
	res, err := st.Publish(dr, pr, sum)
	if err != nil {
		t.Fatal(err)
	}
	return res.Published.Revision
}
