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

	ErrInvalidExceptionRequest = errors.New("invalid exception request")
	ErrTupleOutsideDomain      = errors.New("tuple is not a concrete tuple of the published finite domain")
	ErrTupleNotDenied          = errors.New("tuple is not denied by the currently published policy: only a currently denied tuple can be emergency-allowed")
	ErrExceptionAlreadyActive  = errors.New("an active emergency exception already covers this tuple")
)

// Emergency exception TTL bounds, in minutes.
const (
	MinExceptionTTLMinutes = 1
	MaxExceptionTTLMinutes = 60
	// MaxExceptionReason bounds the mandatory justification text.
	MaxExceptionReason = 300
)

// Versioned bundles a validated document with its monotonic revision.
type Versioned struct {
	Document *policy.Document `json:"document"`
	Revision int              `json:"revision"`
}

// Store is the in-memory, mutex-protected policy database.
//
// Draft and published revisions are independent counters: each draft
// save bumps the draft revision; each successful publish bumps the
// published revision (and the published content is replaced wholesale).
//
// Emergency exceptions are pinned to the published revision they were
// adjudicated against: every create / expiry check / publish decision is
// taken under the same mu that guards the revisions, so an exception and
// the decision it overrides always come from one consistent version
// snapshot. There is deliberately no background reaper: expiry is judged
// lazily, under the lock, on every access.
type Store struct {
	mu        sync.Mutex
	now       func() time.Time
	draft     Versioned
	published Versioned
	// exceptions holds only ACTIVE exceptions, keyed by exception ID.
	// Expired entries are pruned under the lock on every access; publish
	// and demo reset clear the whole map because every entry is pinned to
	// a published revision that is no longer current.
	exceptions map[string]*exception
	// seq assigns unique exception ids.
	seq uint64
}

// New seeds draft and published with the same initial document.
func New(initial *policy.Document) (*Store, error) {
	return newStore(initial, time.Now)
}

// NewWithClock is New with an injected clock, used by tests for exact
// expiry-boundary determinism.
func NewWithClock(initial *policy.Document, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	return newStore(initial, now)
}

func newStore(initial *policy.Document, now func() time.Time) (*Store, error) {
	eng, err := policy.NewEngine(initial)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidDocument, err)
	}
	doc := eng.Document()
	return &Store{
		now:        now,
		draft:      Versioned{Document: doc, Revision: 1},
		published:  Versioned{Document: doc.Clone(), Revision: 1},
		exceptions: map[string]*exception{},
	}, nil
}

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
}

// Decisions evaluates the whole finite domain of the requested version.
//
// The draft path computes the draft rules only — emergency exceptions
// never touch draft decisions or draft-vs-published previews, so students
// cannot mistake a temporary override for an unpublished draft rule.
//
// The published path takes a single consistent snapshot under mu
// (document, revision, and the exception set AFTER lazy expiry pruning)
// and applies exceptions from that same snapshot. The decision and its
// evidence therefore always describe one version: a row is never allowed
// by an exception while its evidence shows a bare rule deny, and an
// exception pinned to an older published revision can never appear.
func (s *Store) Decisions(which string) (int, []DecisionRow, error) {
	if which == "draft" {
		s.mu.Lock()
		doc, rev := s.draft.Document.Clone(), s.draft.Revision
		s.mu.Unlock()

		eng, err := policy.NewEngine(doc)
		if err != nil {
			return 0, nil, err
		}
		rows := make([]DecisionRow, 0)
		for _, t := range eng.Domain() {
			rows = append(rows, DecisionRow{Tuple: t, Evidence: eng.Decide(t)})
		}
		return rev, rows, nil
	}

	snap, err := s.publishedSnapshot()
	if err != nil {
		return 0, nil, err
	}
	rows := make([]DecisionRow, 0)
	for _, t := range snap.eng.Domain() {
		rows = append(rows, DecisionRow{Tuple: t, Evidence: snap.decide(t)})
	}
	return snap.rev, rows, nil
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

	// Every exception was adjudicated against the PREVIOUS published
	// revision. They cannot attach to the new one: clear them all inside
	// the same lock that swapped the version, so the store can never
	// expose a new revision alongside an exception pinned to an old one.
	s.exceptions = map[string]*exception{}

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
