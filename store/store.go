package store

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"accesssim/policy"
)

// Sentinel errors mapped to HTTP statuses by the API layer.
var (
	ErrInvalidDocument    = errors.New("invalid document")
	ErrInvalidSummary     = errors.New("invalid preview summary")
	ErrDraftConflict      = errors.New("draft revision mismatch: preview is stale, re-preview the current draft")
	ErrPublishedConflict  = errors.New("published revision mismatch: another client published in the meantime, re-preview")
	ErrSummaryHashInvalid = errors.New("summary hash does not match its content: refusing to publish an unpreviewed or tampered version")
	ErrSummaryMismatch    = errors.New("summary content differs from the freshly enumerated preview for those revisions")
)

// Versioned bundles a validated document with its monotonic revision.
type Versioned struct {
	Document *policy.Document `json:"document"`
	Revision int              `json:"revision"`
}

// Store is the in-memory, mutex-protected policy database.
// Draft and published revisions are independent counters: each draft
// save bumps the draft revision; each successful publish bumps the
// published revision (and the published content is replaced wholesale).
//
// Emergency exceptions are scoped to exactly one published revision:
// every publish and demo reset clears them. The single mutex s.mu is
// the shared version adjudication point — exception create/expire
// decisions, matrix enumeration and publishing all take it, so the UI
// can never observe a half-applied state (e.g. an allowed cell whose
// evidence still looks like a pure rule deny).
type Store struct {
	mu        sync.Mutex
	draft     Versioned
	published Versioned

	// Emergency exceptions pinned to the current published revision.
	exceptions []*EmergencyException
	// exceptionSeq is store-global; ids must never be reused after a
	// publish/reset purge.
	exceptionSeq int64

	// now is injectable for deterministic TTL-boundary tests.
	now func() time.Time
}

// New seeds draft and published with the same initial document.
func New(initial *policy.Document) (*Store, error) {
	return NewWithClock(initial, time.Now)
}

// NewWithClock seeds the store with an injected clock. Production code
// uses New (wall clock); tests pass a controllable clock to assert the
// 1–60 minute TTL boundaries without sleeping.
func NewWithClock(initial *policy.Document, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	eng, err := policy.NewEngine(initial)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidDocument, err)
	}
	doc := eng.Document()
	return &Store{
		draft:     Versioned{Document: doc, Revision: 1},
		published: Versioned{Document: doc.Clone(), Revision: 1},
		now:       now,
	}, nil
}

// ClockNow reports the store's current simulated time.
func (s *Store) ClockNow() time.Time { return s.now() }

// Snapshot returns deep copies so callers can serialize without the lock.
func (s *Store) Snapshot() (draft, published Versioned) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Versioned{Document: s.draft.Document.Clone(), Revision: s.draft.Revision},
		Versioned{Document: s.published.Document.Clone(), Revision: s.published.Revision}
}

// SaveDraft validates and replaces the draft, bumping its revision.
// The published version is untouched.
func (s *Store) SaveDraft(doc *policy.Document) (Versioned, error) {
	eng, err := policy.NewEngine(doc)
	if err != nil {
		return Versioned{}, fmt.Errorf("%w: %v", ErrInvalidDocument, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.draft = Versioned{Document: eng.Document(), Revision: s.draft.Revision + 1}
	return Versioned{Document: s.draft.Document.Clone(), Revision: s.draft.Revision}, nil
}

// Preview enumerates the current draft against the current published
// version. The returned summary is only valid for this exact revision
// pair; any edit or publish invalidates it.
func (s *Store) Preview() (*policy.Summary, int, int, error) {
	s.mu.Lock()
	draftDoc := s.draft.Document.Clone()
	draftRev := s.draft.Revision
	pubDoc := s.published.Document.Clone()
	pubRev := s.published.Revision
	s.mu.Unlock()

	draftEng, err := policy.NewEngine(draftDoc)
	if err != nil {
		return nil, 0, 0, err
	}
	pubEng, err := policy.NewEngine(pubDoc)
	if err != nil {
		return nil, 0, 0, err
	}
	sum, err := policy.BuildSummary(draftEng, draftRev, pubEng, pubRev)
	if err != nil {
		return nil, 0, 0, err
	}
	return sum, draftRev, pubRev, nil
}

// DecisionRow is one evaluated tuple with its evidence, for the UI grid.
type DecisionRow struct {
	Tuple    policy.Tuple    `json:"tuple"`
	Evidence policy.Evidence `json:"evidence"`
	// Exception is non-nil exactly when Evidence is an active emergency
	// override. It travels in the same response payload as Evidence, so
	// matrix colour and evidence view can never be served from two
	// different adjudication moments.
	Exception *EmergencyException `json:"exception,omitempty"`
}

// Decisions evaluates the whole finite domain of the requested version.
//
// The published view overlays live (unexpired) emergency exceptions: a
// currently-denied tuple with an active exception is reported as a
// temporary allow carrying both the original deny rule evidence and the
// exception identity. The draft view is computed from the draft rules
// alone — exceptions never alter draft decisions or previews.
func (s *Store) Decisions(which string) (int, []DecisionRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var doc *policy.Document
	var rev int
	switch which {
	case "draft":
		doc, rev = s.draft.Document.Clone(), s.draft.Revision
	default:
		doc, rev = s.published.Document.Clone(), s.published.Revision
	}

	eng, err := policy.NewEngine(doc)
	if err != nil {
		return 0, nil, err
	}
	now := s.now()
	// Prune-then-read within the same lock acquisition: expiry and the
	// returned matrix share one version adjudication, no background
	// task required.
	live := s.pruneExceptionsLocked(now)
	byTuple := map[policy.Tuple]*EmergencyException{}
	if which != "draft" {
		for _, ex := range live {
			byTuple[ex.Tuple] = ex
		}
	}

	rows := make([]DecisionRow, 0)
	for _, t := range eng.Domain() {
		ev := eng.Decide(t)
		row := DecisionRow{Tuple: t, Evidence: ev}
		if ex, ok := byTuple[t]; ok {
			row.Evidence = policy.ApplyEmergencyException(ev, ex.ID)
			cp := *ex
			row.Exception = &cp
		}
		rows = append(rows, row)
	}
	return rev, rows, nil
}

// Publish atomically replaces the published document, but only if ALL
// of the following hold:
//
//  1. req.DraftRevision equals the current draft revision (the draft
//     was not edited after the preview);
//  2. req.PublishedRevision equals the current published revision
//     (no other client published in the meantime);
//  3. the summary hash matches its own content (untampered);
//  4. the summary content equals a fresh enumeration of exactly that
//     revision pair (the summary is genuinely about these versions).
//
// This guarantees a client can never publish a mixed/unpreviewed state.
type PublishResult struct {
	Published     Versioned       `json:"published"`
	DraftRevision int             `json:"draftRevision"`
	Summary       *policy.Summary `json:"summary"`
}

func (s *Store) Publish(draftRev, pubRev int, sum *policy.Summary) (*PublishResult, error) {
	if sum == nil {
		return nil, ErrInvalidSummary
	}
	ok, err := sum.VerifyHash()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSummaryHashInvalid, err)
	}
	if !ok {
		return nil, ErrSummaryHashInvalid
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if sum.DraftRevision != draftRev || sum.PublishedRevision != pubRev {
		return nil, ErrInvalidSummary
	}
	if draftRev != s.draft.Revision {
		return nil, ErrDraftConflict
	}
	if pubRev != s.published.Revision {
		return nil, ErrPublishedConflict
	}

	// Re-enumerate inside the lock and compare to the presented summary
	// so the preview must describe precisely what is about to go live.
	draftEng, err := policy.NewEngine(s.draft.Document)
	if err != nil {
		return nil, err
	}
	pubEng, err := policy.NewEngine(s.published.Document)
	if err != nil {
		return nil, err
	}
	fresh, err := policy.BuildSummary(draftEng, draftRev, pubEng, pubRev)
	if err != nil {
		return nil, err
	}
	if !summariesEqual(fresh, sum) {
		return nil, ErrSummaryMismatch
	}

	newPublished := Versioned{
		Document: s.draft.Document.Clone(),
		Revision: s.published.Revision + 1,
	}
	s.published = newPublished
	// A new published revision invalidates every emergency exception
	// immediately, in the same critical section: a late create carrying
	// the old revision can never attach to this revision, and a matrix
	// read after publish can never see an exception for old rules.
	s.exceptions = nil

	sumOut := *fresh
	return &PublishResult{
		Published:     Versioned{Document: newPublished.Document.Clone(), Revision: newPublished.Revision},
		DraftRevision: s.draft.Revision,
		Summary:       &sumOut,
	}, nil
}

// summariesEqual compares the hashes; both inputs carry hashes computed
// over the same canonical shape.
func summariesEqual(a, b *policy.Summary) bool {
	return a.Hash != "" && a.Hash == b.Hash
}
