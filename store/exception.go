package store

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"accesssim/policy"
)

// exception is the stored, internal form of one emergency override.
// It is pinned to exactly one published revision and one concrete tuple.
type exception struct {
	id      string
	tuple   policy.Tuple
	reason  string
	rev     int
	created time.Time
	// expires is the exclusive expiry instant: the exception is active
	// while now.Before(expires); at now == expires it has expired.
	expires time.Time
}

// ExceptionView is the serialized form of an active exception.
type ExceptionView struct {
	ID                string       `json:"id"`
	Tuple             policy.Tuple `json:"tuple"`
	Reason            string       `json:"reason"`
	CreatedAt         time.Time    `json:"createdAt"`
	ExpiresAt         time.Time    `json:"expiresAt"`
	PublishedRevision int          `json:"publishedRevision"`
}

func (ex *exception) view() ExceptionView {
	return ExceptionView{
		ID:                ex.id,
		Tuple:             ex.tuple,
		Reason:            ex.reason,
		CreatedAt:         ex.created.UTC(),
		ExpiresAt:         ex.expires.UTC(),
		PublishedRevision: ex.rev,
	}
}

// ExceptionRequest creates a simulated emergency exception for one exact
// (role, resource, action) tuple with a mandatory reason and a TTL of
// 1..60 minutes.
type ExceptionRequest struct {
	Role       string `json:"role"`
	Resource   string `json:"resource"`
	Action     string `json:"action"`
	Reason     string `json:"reason"`
	TTLMinutes int    `json:"ttlMinutes"`
}

// publishedSnapshot is one consistent view of the published world: the
// document/revision, the engine built from that document, the clock
// reading taken for expiry adjudication, and the set of exceptions that
// were still active at that reading. All of it is gathered under the
// store lock, so decision and evidence always describe the same revision.
type publishedSnapshot struct {
	eng     *policy.Engine
	rev     int
	now     time.Time
	byTuple map[policy.Tuple]*exception
}

// pruneExpiredLocked drops every exception whose TTL has elapsed.
// Expiry is exclusive: expiresAt <= now means gone. Called only with mu
// held; this is the only "reaper" — there is no background goroutine.
func (s *Store) pruneExpiredLocked(now time.Time) {
	for id, ex := range s.exceptions {
		if !now.Before(ex.expires) {
			delete(s.exceptions, id)
		}
	}
}

// publishedSnapshotLocked takes the consistent published snapshot.
// Called with mu held.
func (s *Store) publishedSnapshotLocked(now time.Time) (*publishedSnapshot, error) {
	eng, err := policy.NewEngine(s.published.Document)
	if err != nil {
		return nil, err
	}
	byTuple := make(map[policy.Tuple]*exception, len(s.exceptions))
	for _, ex := range s.exceptions {
		byTuple[ex.tuple] = ex
	}
	return &publishedSnapshot{
		eng:     eng,
		rev:     s.published.Revision,
		now:     now,
		byTuple: byTuple,
	}, nil
}

// publishedSnapshot prunes expired exceptions under the lock and returns
// the consistent snapshot used by every published decision read.
func (s *Store) publishedSnapshot() (*publishedSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pruneExpiredLocked(now)
	return s.publishedSnapshotLocked(now)
}

// decide returns the published decision for one tuple of the snapshot:
// the pure published ruling, flipped to a temporary allow ONLY when an
// active exception pinned to THIS snapshot's revision covers it. The
// override evidence keeps the full original rule chain and embeds the
// untouched base ruling, so the UI never mixes an allowed cell with a
// bare rule-deny explanation.
func (snap *publishedSnapshot) decide(t policy.Tuple) policy.Evidence {
	base := snap.eng.Decide(t)
	ex := snap.byTuple[t]
	if ex == nil || base.Decision != policy.EffectDeny {
		return base
	}
	return withException(base, ex, snap.rev)
}

// withException builds the override evidence from a deny base ruling.
// Defensively only called on deny evidence; callers guarantee the match.
func withException(base policy.Evidence, ex *exception, rev int) policy.Evidence {
	baseCopy := base
	return policy.Evidence{
		Decision:     policy.EffectAllow,
		Reason:       policy.ReasonEmergencyExcept,
		Considered:   base.Considered,
		Winners:      base.Winners,
		BaseEvidence: &baseCopy,
		Exception: &policy.ExceptionRef{
			ID:                ex.id,
			PublishedRevision: rev,
			Reason:            ex.reason,
			ExpiresAt:         ex.expires.UTC().Format(time.RFC3339),
		},
	}
}

// tupleInDomain reports whether t is a concrete tuple of the engine's
// finite domain.
func tupleInDomain(eng *policy.Engine, t policy.Tuple) bool {
	for _, d := range eng.Domain() {
		if d == t {
			return true
		}
	}
	return false
}

// CreateException adjudicates the request against the CURRENTLY published
// revision, entirely under the store lock:
//
//  1. expired exceptions are pruned first (lazy expiry, no background
//     task);
//  2. the tuple must be a concrete tuple of the published finite domain
//     (no wildcards, no draft-only names);
//  3. the current published ruling for that exact tuple must be deny —
//     the re-adjudication happens inside the same lock, against the
//     revision the exception is then pinned to, so a request arriving
//     just after a publish can only attach to the new revision if the new
//     revision really denies the tuple;
//  4. no other active exception may cover the same tuple.
//
// On success it returns the exception view and the NEW effective
// published evidence (temporary allow plus the preserved base evidence).
func (s *Store) CreateException(req ExceptionRequest) (ExceptionView, policy.Evidence, error) {
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		return ExceptionView{}, policy.Evidence{}, fmt.Errorf("%w: reason is required", ErrInvalidExceptionRequest)
	}
	if utf8.RuneCountInString(reason) > MaxExceptionReason {
		return ExceptionView{}, policy.Evidence{}, fmt.Errorf("%w: reason must be at most %d characters", ErrInvalidExceptionRequest, MaxExceptionReason)
	}
	if req.TTLMinutes < MinExceptionTTLMinutes || req.TTLMinutes > MaxExceptionTTLMinutes {
		return ExceptionView{}, policy.Evidence{}, fmt.Errorf("%w: ttlMinutes must be within [%d,%d]", ErrInvalidExceptionRequest, MinExceptionTTLMinutes, MaxExceptionTTLMinutes)
	}
	t := policy.Tuple{Role: req.Role, Resource: req.Resource, Action: req.Action}
	if t.Role == "" || t.Resource == "" || t.Action == "" ||
		t.Role == policy.Wildcard || t.Resource == policy.Wildcard || t.Action == policy.Wildcard {
		return ExceptionView{}, policy.Evidence{}, fmt.Errorf("%w: role, resource and action must name one concrete tuple (no wildcards)", ErrInvalidExceptionRequest)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	s.pruneExpiredLocked(now)

	snap, err := s.publishedSnapshotLocked(now)
	if err != nil {
		return ExceptionView{}, policy.Evidence{}, err
	}
	if !tupleInDomain(snap.eng, t) {
		return ExceptionView{}, policy.Evidence{}, ErrTupleOutsideDomain
	}
	base := snap.eng.Decide(t)
	if base.Decision != policy.EffectDeny {
		return ExceptionView{}, base, ErrTupleNotDenied
	}
	if ex := snap.byTuple[t]; ex != nil {
		return ExceptionView{}, withException(base, ex, snap.rev), ErrExceptionAlreadyActive
	}

	s.seq++
	ex := &exception{
		id:      fmt.Sprintf("exc-p%d-%d", snap.rev, s.seq),
		tuple:   t,
		reason:  reason,
		rev:     snap.rev,
		created: now,
		expires: now.Add(time.Duration(req.TTLMinutes) * time.Minute),
	}
	s.exceptions[ex.id] = ex

	return ex.view(), withException(base, ex, snap.rev), nil
}

// ListExceptions returns all currently active exceptions (expired ones
// are pruned first), sorted by expiry ascending then id.
func (s *Store) ListExceptions() (publishedRev int, out []ExceptionView, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pruneExpiredLocked(now)
	out = make([]ExceptionView, 0, len(s.exceptions))
	for _, ex := range s.exceptions {
		out = append(out, ex.view())
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ExpiresAt.Equal(out[j].ExpiresAt) {
			return out[i].ExpiresAt.Before(out[j].ExpiresAt)
		}
		return out[i].ID < out[j].ID
	})
	return s.published.Revision, out, nil
}

// Replace atomically swaps a store's entire contents for those of other
// (used by the demo reset). The receiver keeps its own mutex; nothing
// containing a lock is copied. All existing exceptions are dropped along
// with the old contents.
func (s *Store) Replace(other *Store) {
	other.mu.Lock()
	defer other.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()

	s.now = other.now
	s.draft = Versioned{Document: other.draft.Document.Clone(), Revision: other.draft.Revision}
	s.published = Versioned{Document: other.published.Document.Clone(), Revision: other.published.Revision}
	s.exceptions = map[string]*exception{}
	s.seq = 0
}
