package store

import (
	"errors"
	"sync"
	"testing"
	"time"

	"accesssim/policy"
)

// clock is a manually controlled time source for deterministic TTL tests.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newClockStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)}
	st, err := NewWithClock(baseDoc(), c.now)
	if err != nil {
		t.Fatal(err)
	}
	return st, c
}

// deniedTuple is editor/doc/read: editor inherits only base in the test
// diamond, so only the wildcard base-deny rule matches -> pure deny.
var deniedTuple = policy.Tuple{Role: "editor", Resource: "doc", Action: "read"}

// allowedTuple is viewer/doc/read: viewer-read allows it at p10.
var allowedTuple = policy.Tuple{Role: "viewer", Resource: "doc", Action: "read"}

func findRow(rows []DecisionRow, t policy.Tuple) DecisionRow {
	for _, r := range rows {
		if r.Tuple == t {
			return r
		}
	}
	return DecisionRow{}
}

// TestExceptionCreateReAdjudicatesDeny is the core happy path: creation
// must re-adjudicate inside the published finite domain, only a current
// deny may be overridden, and afterwards published decisions return a
// temporary allow carrying BOTH the original deny rule evidence and the
// exception identity. Draft decisions and previews stay rule-only.
func TestExceptionCreateReAdjudicatesDeny(t *testing.T) {
	st, c := newClockStore(t)

	res, err := st.CreateEmergencyException(&CreateExceptionRequest{
		Tuple:             deniedTuple,
		Reason:            "incident drill: auditor needs doc read for 5 minutes",
		TTLMinutes:        5,
		PublishedRevision: 1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ex := res.Exception
	if ex.ID == "" || ex.PublishedRevision != 1 {
		t.Fatalf("exception = %+v, want id and pin to published revision 1", ex)
	}
	if !ex.ExpiresAt.Equal(c.t.Add(5 * time.Minute)) {
		t.Fatalf("expiresAt = %v, want %v", ex.ExpiresAt, c.t.Add(5*time.Minute))
	}

	// The create response itself must contain one coherent adjudication:
	// allow + exception id + original deny evidence, never a mix.
	d := res.Decision
	if d.Evidence.Decision != policy.EffectAllow ||
		d.Evidence.Reason != policy.ReasonEmergencyAllow ||
		d.Evidence.ExceptionID != ex.ID {
		t.Fatalf("create decision = %+v, want emergency allow tagged %s", d.Evidence, ex.ID)
	}
	if d.Evidence.OriginalReason != policy.ReasonSingleWinner ||
		len(d.Evidence.Winners) != 1 || d.Evidence.Winners[0].RuleID != "base-deny" {
		t.Fatalf("original deny evidence not preserved: %+v", d.Evidence)
	}
	if d.Exception == nil || d.Exception.ID != ex.ID {
		t.Fatal("decision row must carry the exception object alongside evidence")
	}

	// Published matrix: temporary allow with attached identity/evidence.
	_, pubRows, err := st.Decisions("published")
	if err != nil {
		t.Fatal(err)
	}
	pr := findRow(pubRows, deniedTuple)
	if pr.Evidence.Decision != "allow" || pr.Evidence.ExceptionID != ex.ID || pr.Exception == nil {
		t.Fatalf("published row = %+v, want overridden allow with exception", pr)
	}
	if pr.Evidence.Winners[0].RuleID != "base-deny" {
		t.Fatalf("published row lost original deny evidence: %+v", pr.Evidence)
	}

	// Draft matrix: exceptions never leak into draft adjudication.
	_, draftRows, err := st.Decisions("draft")
	if err != nil {
		t.Fatal(err)
	}
	dr := findRow(draftRows, deniedTuple)
	if dr.Evidence.Decision != "deny" || dr.Evidence.ExceptionID != "" || dr.Exception != nil {
		t.Fatalf("draft row = %+v, exceptions must not alter draft", dr)
	}

	// Preview/diff also stays rule-only (no phantom new denies/allows).
	sum, _, _, err := st.Preview()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range sum.NewAllows {
		if e.Tuple == deniedTuple {
			t.Fatal("exception leaked into preview as a new allow")
		}
	}

	// Listing reports it.
	_, _, listed, err := st.ListExceptions()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != ex.ID {
		t.Fatalf("listed = %+v, want single %s", listed, ex.ID)
	}
}

// TestExceptionRejectsAllowedTuple: only a tuple currently denied by
// the published policy can get an exception — the store re-decides
// rather than trusting the request.
func TestExceptionRejectsAllowedTuple(t *testing.T) {
	st, _ := newClockStore(t)
	_, err := st.CreateEmergencyException(&CreateExceptionRequest{
		Tuple: allowedTuple, Reason: "x", TTLMinutes: 5, PublishedRevision: 1,
	})
	if !errors.Is(err, ErrTupleNotDenied) {
		t.Fatalf("err = %v, want ErrTupleNotDenied", err)
	}
	// Nothing was created.
	_, _, listed, _ := st.ListExceptions()
	if len(listed) != 0 {
		t.Fatalf("listed = %d, want 0", len(listed))
	}
}

// TestExceptionRejectsOutOfDomainTuple: an invalid tuple is rejected
// wholesale; no partial exception state may exist afterwards.
func TestExceptionRejectsOutOfDomainTuple(t *testing.T) {
	st, _ := newClockStore(t)
	bad := []policy.Tuple{
		{Role: "ghost", Resource: "doc", Action: "read"},
		{Role: "editor", Resource: "nope", Action: "read"},
		{Role: "editor", Resource: "doc", Action: "execute"},
		{Role: "", Resource: "doc", Action: "read"},
	}
	for _, tup := range bad {
		_, err := st.CreateEmergencyException(&CreateExceptionRequest{
			Tuple: tup, Reason: "x", TTLMinutes: 5, PublishedRevision: 1,
		})
		if !errors.Is(err, ErrInvalidException) {
			t.Fatalf("tuple %+v err = %v, want ErrInvalidException", tup, err)
		}
	}
	_, _, listed, _ := st.ListExceptions()
	if len(listed) != 0 {
		t.Fatalf("listed = %d, want 0", len(listed))
	}
}

// TestExceptionTTLValidation covers the 1–60 minute bounds.
func TestExceptionTTLValidation(t *testing.T) {
	st, _ := newClockStore(t)
	for _, ttl := range []int{0, -1, 61, 1000} {
		_, err := st.CreateEmergencyException(&CreateExceptionRequest{
			Tuple: deniedTuple, Reason: "x", TTLMinutes: ttl, PublishedRevision: 1,
		})
		if !errors.Is(err, ErrInvalidException) {
			t.Fatalf("ttl %d err = %v, want ErrInvalidException", ttl, err)
		}
	}
	for _, ttl := range []int{1, 60} {
		st, _ := newClockStore(t)
		if _, err := st.CreateEmergencyException(&CreateExceptionRequest{
			Tuple: deniedTuple, Reason: "boundary", TTLMinutes: ttl, PublishedRevision: 1,
		}); err != nil {
			t.Fatalf("ttl %d: %v", ttl, err)
		}
	}

	// Missing reason / revision are rejected too.
	st, _ = newClockStore(t)
	if _, err := st.CreateEmergencyException(&CreateExceptionRequest{
		Tuple: deniedTuple, Reason: "   ", TTLMinutes: 5, PublishedRevision: 1,
	}); !errors.Is(err, ErrInvalidException) {
		t.Fatalf("blank reason err = %v", err)
	}
	if _, err := st.CreateEmergencyException(&CreateExceptionRequest{
		Tuple: deniedTuple, Reason: "x", TTLMinutes: 5, PublishedRevision: 0,
	}); !errors.Is(err, ErrInvalidException) {
		t.Fatalf("missing revision err = %v", err)
	}
}

// TestExceptionExpiresWithoutBackgroundTask verifies the lazy expiry
// boundary semantics: active at created+(ttl-1ns), restored to the
// original deny exactly at the expiration instant, with no goroutine.
func TestExceptionExpiresWithoutBackgroundTask(t *testing.T) {
	st, c := newClockStore(t)
	ex, err := st.CreateEmergencyException(&CreateExceptionRequest{
		Tuple: deniedTuple, Reason: "boundary drill", TTLMinutes: 1, PublishedRevision: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	// One nanosecond before expiry: still overridden.
	c.advance(time.Minute - time.Nanosecond)
	_, rows, _ := st.Decisions("published")
	if r := findRow(rows, deniedTuple); r.Evidence.Decision != "allow" ||
		r.Evidence.ExceptionID != ex.Exception.ID {
		t.Fatalf("pre-expiry row = %+v, want active override", r)
	}
	if n := len(mustList(st)); n != 1 {
		t.Fatalf("pre-expiry list len = %d, want 1", n)
	}

	// Exactly at the expiry instant: original deny is back and the
	// exception has been lazily purged.
	c.advance(time.Nanosecond)
	_, rows, _ = st.Decisions("published")
	r := findRow(rows, deniedTuple)
	if r.Evidence.Decision != "deny" || r.Evidence.ExceptionID != "" || r.Exception != nil {
		t.Fatalf("expired row = %+v, want plain rule deny with no exception traces", r)
	}
	if n := len(mustList(st)); n != 0 {
		t.Fatalf("post-expiry list len = %d, want 0", n)
	}

	// A new exception for the same tuple is allowed after expiry, and
	// gets a fresh id (ids are never reused).
	ex2, err := st.CreateEmergencyException(&CreateExceptionRequest{
		Tuple: deniedTuple, Reason: "again", TTLMinutes: 1, PublishedRevision: 1,
	})
	if err != nil {
		t.Fatalf("recreate after expiry: %v", err)
	}
	if ex2.Exception.ID == ex.Exception.ID {
		t.Fatal("exception id reused after expiry")
	}
}

// TestExceptionDuplicateRejected: while active, a second create for the
// same tuple is refused.
func TestExceptionDuplicateRejected(t *testing.T) {
	st, _ := newClockStore(t)
	if _, err := st.CreateEmergencyException(&CreateExceptionRequest{
		Tuple: deniedTuple, Reason: "first", TTLMinutes: 5, PublishedRevision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateEmergencyException(&CreateExceptionRequest{
		Tuple: deniedTuple, Reason: "second", TTLMinutes: 5, PublishedRevision: 1,
	}); !errors.Is(err, ErrExceptionExists) {
		t.Fatalf("duplicate err = %v, want ErrExceptionExists", err)
	}
}

// TestExceptionInvalidatedByPublish: publishing a new revision drops all
// exceptions atomically, and a late create stamped with the old
// published revision cannot attach to the new revision.
func TestExceptionInvalidatedByPublish(t *testing.T) {
	st, c := newClockStore(t)
	if _, err := st.CreateEmergencyException(&CreateExceptionRequest{
		Tuple: deniedTuple, Reason: "drill", TTLMinutes: 10, PublishedRevision: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// Save a draft and publish it (draft rev 2 / pub rev 1 -> pub rev 2).
	draft := baseDoc()
	draft.Rules = append(draft.Rules, policy.Rule{
		ID: "editor-read", Role: "editor", Resource: "doc", Action: "read",
		Priority: 20, Effect: policy.EffectAllow,
	})
	if _, err := st.SaveDraft(draft); err != nil {
		t.Fatal(err)
	}
	sum, dr, pr, err := st.Preview()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Publish(dr, pr, sum); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Clock unchanged, so the old exception would still be time-valid —
	// but the revision moved, so it must be gone regardless of time.
	_ = c
	_, rows, _ := st.Decisions("published")
	r := findRow(rows, deniedTuple)
	// New published rules allow this tuple at p20 — via a RULE, with no
	// exception identity attached.
	if r.Evidence.Decision != "allow" || r.Evidence.ExceptionID != "" || r.Exception != nil {
		t.Fatalf("post-publish row = %+v, want plain rule allow, no stale exception", r)
	}
	if r.Evidence.Reason != policy.ReasonSingleWinner ||
		r.Evidence.Winners[0].RuleID != "editor-read" {
		t.Fatalf("post-publish evidence = %+v, want editor-read rule", r.Evidence)
	}
	if n := len(mustList(st)); n != 0 {
		t.Fatalf("post-publish list len = %d, want 0", n)
	}

	// A create request that was prepared against p1 and arrives after
	// the publish is rejected even though its tuple is now denied... it
	// is allowed here, so pick a tuple that stays denied; either way the
	// revision guard fires FIRST.
	late := policy.Tuple{Role: "base", Resource: "billing", Action: "write"}
	_, err = st.CreateEmergencyException(&CreateExceptionRequest{
		Tuple: late, Reason: "late", TTLMinutes: 5, PublishedRevision: 1,
	})
	if !errors.Is(err, ErrExceptionRevisionMoved) {
		t.Fatalf("late create err = %v, want ErrExceptionRevisionMoved", err)
	}

	// The same request against the CURRENT revision gets re-adjudicated
	// (base/billing/write is denied by base-deny) and succeeds.
	res, err := st.CreateEmergencyException(&CreateExceptionRequest{
		Tuple: late, Reason: "late", TTLMinutes: 5, PublishedRevision: 2,
	})
	if err != nil {
		t.Fatalf("re-adjudicated create: %v", err)
	}
	if res.Exception.PublishedRevision != 2 {
		t.Fatalf("new exception pinned to %d, want 2", res.Exception.PublishedRevision)
	}
}

// TestConcurrentPublishVsCreate interleaves publishes and creates under
// -race and asserts the store-level invariant: whenever the published
// revision has moved past 1, no p1 exception can be observable on any
// decision row (no attaching to a new revision) and create/publish never
// corrupt each other.
func TestConcurrentPublishVsCreate(t *testing.T) {
	st, _ := newClockStore(t)

	// One valid summary for the (draft=1, published=1) identical pair:
	// only one publish with it can succeed; the loser gets
	// ErrPublishedConflict, which is expected.
	eng := mustEngine(t, baseDoc())
	sum, err := policy.BuildSummary(eng, 1, eng, 1)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var createOK, createMoved int

	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = st.Publish(1, 1, sum)
	}()
	go func() {
		defer wg.Done()
		for range 100 {
			_, err := st.CreateEmergencyException(&CreateExceptionRequest{
				Tuple: deniedTuple, Reason: "race", TTLMinutes: 5, PublishedRevision: 1,
			})
			mu.Lock()
			switch {
			case err == nil:
				createOK++
			case errors.Is(err, ErrExceptionRevisionMoved):
				createMoved++
			case errors.Is(err, ErrExceptionExists):
				// Another create iteration won first; fine.
			default:
				t.Errorf("unexpected create error: %v", err)
			}
			mu.Unlock()
		}
	}()
	wg.Wait()

	// Final state: published revision is 2. Nothing pinned to p1 may
	// survive on the matrix, and exception identity is always paired
	// with its exception object (no mixed state).
	_, rows, _ := st.Decisions("published")
	for _, r := range rows {
		hasID := r.Evidence.ExceptionID != ""
		hasObj := r.Exception != nil
		if hasID != hasObj {
			t.Fatalf("mixed state on %+v: exceptionId=%v object=%v", r.Tuple, hasID, hasObj)
		}
		if hasID && r.Exception.PublishedRevision == 1 {
			t.Fatalf("stale p1 exception observable after publish: %+v", r)
		}
	}
}

// TestDecisionsNoMixedState asserts the page-level invariant on every
// row of every enumeration while an exception is live: an emergency
// allow cell ALWAYS carries exception identity + object, and any row
// carrying an exception is an emergency allow. A pure-rule-deny
// evidence next to an exception-released cell cannot happen.
func TestDecisionsNoMixedState(t *testing.T) {
	st, c := newClockStore(t)
	if _, err := st.CreateEmergencyException(&CreateExceptionRequest{
		Tuple: deniedTuple, Reason: "drill", TTLMinutes: 3, PublishedRevision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		_, rows, _ := st.Decisions("published")
		for _, r := range rows {
			hasID := r.Evidence.ExceptionID != ""
			hasObj := r.Exception != nil
			if hasID != hasObj {
				t.Fatalf("mixed state on %+v: exceptionId=%v object=%v", r.Tuple, hasID, hasObj)
			}
			if hasID && (r.Evidence.Decision != "allow" ||
				r.Evidence.Reason != policy.ReasonEmergencyAllow ||
				r.Exception.PublishedRevision != 1) {
				t.Fatalf("inconsistent exception row: %+v", r)
			}
			if !hasID && hasObj {
				t.Fatalf("exception object without identity: %+v", r)
			}
		}
		c.advance(time.Minute)
	}
}

func mustList(st *Store) []EmergencyException {
	_, _, l, err := st.ListExceptions()
	if err != nil {
		panic(err)
	}
	return l
}
