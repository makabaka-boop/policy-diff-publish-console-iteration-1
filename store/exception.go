package store

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"accesssim/policy"
)

// Simulated emergency exception bounds. This is a teaching lab: the
// reasons are mandatory, and the lifetime is deliberately short.
const (
	MinExceptionTTLMinutes = 1
	MaxExceptionTTLMinutes = 60
	MaxExceptionReasonLen  = 300
)

// Errors surfaced to the API layer.
var (
	ErrInvalidException = errors.New("invalid emergency exception request")
	ErrTupleNotDenied   = errors.New("tuple is currently allowed by the published policy: an emergency exception can only be granted for a tuple the published policy denies")
	ErrExceptionExists  = errors.New("an active emergency exception already covers this tuple")
	// ErrExceptionRevisionMoved is the "late create" race: the create
	// request was adjudicated against (and pinned to) a published
	// revision that has since been replaced by a publish/reset.
	ErrExceptionRevisionMoved = errors.New("published revision moved while creating the exception: re-adjudicate against the current revision")
)

// EmergencyException is a time-boxed temporary allow for exactly one
// (role, resource, action) tuple of one published revision. It is
// intentionally NOT part of either draft or published documents: it is
// ephemeral simulation state, not a rule, and never appears in previews.
type EmergencyException struct {
	ID        string       `json:"id"`
	Tuple     policy.Tuple `json:"tuple"`
	Reason    string       `json:"reason"`
	CreatedAt time.Time    `json:"createdAt"`
	ExpiresAt time.Time    `json:"expiresAt"`
	// PublishedRevision pins the exception to the exact published
	// revision it was re-adjudicated against. A publish or reset bumps
	// the revision and drops the whole set atomically.
	PublishedRevision int `json:"publishedRevision"`
}

// CreateExceptionRequest is the admin-supplied payload. PublishedRevision
// is required: it carries the matrix the admin looked at, and the store
// refuses the create if the live published revision differs, so a
// request delayed across a publish cannot attach to the new revision.
type CreateExceptionRequest struct {
	Tuple             policy.Tuple
	Reason            string
	TTLMinutes        int
	PublishedRevision int
}

// ExceptionResult reports the created exception together with the
// published decision as it now stands and — crucially — the original
// deny evidence, so the client learns all three facts (temporary allow,
// original rule evidence, exception identity) from one adjudication.
type ExceptionResult struct {
	Exception *EmergencyException `json:"exception"`
	Published Versioned           `json:"published"`
	Now       time.Time           `json:"now"`
	Decision  DecisionRow         `json:"decision"`
}

func (s *Store) validateExceptionRequest(req *CreateExceptionRequest) (time.Duration, error) {
	if req == nil {
		return 0, fmt.Errorf("%w: empty request", ErrInvalidException)
	}
	if req.Tuple.Role == "" || req.Tuple.Resource == "" || req.Tuple.Action == "" {
		return 0, fmt.Errorf("%w: role, resource and action are all required", ErrInvalidException)
	}
	if strings.TrimSpace(req.Reason) == "" {
		return 0, fmt.Errorf("%w: a non-empty reason is required", ErrInvalidException)
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if len(req.Reason) > MaxExceptionReasonLen {
		return 0, fmt.Errorf("%w: reason must be at most %d characters", ErrInvalidException, MaxExceptionReasonLen)
	}
	if req.TTLMinutes < MinExceptionTTLMinutes || req.TTLMinutes > MaxExceptionTTLMinutes {
		return 0, fmt.Errorf("%w: ttlMinutes must be within [%d,%d]",
			ErrInvalidException, MinExceptionTTLMinutes, MaxExceptionTTLMinutes)
	}
	if req.PublishedRevision < 1 {
		return 0, fmt.Errorf("%w: publishedRevision is required", ErrInvalidException)
	}
	return time.Duration(req.TTLMinutes) * time.Minute, nil
}

// CreateEmergencyException re-adjudicates the exact tuple inside the
// store's mutex and, only if the CURRENT published decision is deny,
// records a time-boxed temporary allow pinned to the current published
// revision.
//
// Re-adjudication is mandatory: a request claiming a tuple the admin
// "knows" is denied is ignored — the live published document decides.
// Tuples outside the published finite domain are rejected wholesale
// (there is no partial state to create).
func (s *Store) CreateEmergencyException(req *CreateExceptionRequest) (*ExceptionResult, error) {
	ttl, err := s.validateExceptionRequest(req)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	live := s.pruneExceptionsLocked(now)

	// Late request: the matrix the admin saw belongs to an old revision.
	if req.PublishedRevision != s.published.Revision {
		return nil, ErrExceptionRevisionMoved
	}

	eng, err := policy.NewEngine(s.published.Document)
	if err != nil {
		return nil, err
	}
	if !eng.Contains(req.Tuple) {
		return nil, fmt.Errorf("%w: tuple %s/%s/%s is not part of the published finite domain",
			ErrInvalidException, req.Tuple.Role, req.Tuple.Resource, req.Tuple.Action)
	}

	for _, ex := range live {
		if ex.Tuple == req.Tuple {
			return nil, ErrExceptionExists
		}
	}

	original := eng.Decide(req.Tuple)
	if original.Decision != policy.EffectDeny {
		return nil, ErrTupleNotDenied
	}

	s.exceptionSeq++
	ex := &EmergencyException{
		ID:                fmt.Sprintf("ex-%d", s.exceptionSeq),
		Tuple:             req.Tuple,
		Reason:            req.Reason,
		CreatedAt:         now,
		ExpiresAt:         now.Add(ttl),
		PublishedRevision: s.published.Revision,
	}
	s.exceptions = append(s.exceptions, ex)

	row := DecisionRow{
		Tuple:     req.Tuple,
		Evidence:  policy.ApplyEmergencyException(original, ex.ID),
		Exception: ex,
	}
	return &ExceptionResult{
		Exception: ex,
		Published: Versioned{Document: s.published.Document.Clone(), Revision: s.published.Revision},
		Now:       now,
		Decision:  row,
	}, nil
}

// pruneExceptionsLocked drops every exception that has expired at or
// before now and returns the survivors. Expiry is purely lazy: checked
// under the same lock by create, list and matrix enumeration, so an
// expired exception is simply gone the next time anyone looks — no
// background goroutine and no separate sweep moment can disagree with
// a decision.
func (s *Store) pruneExceptionsLocked(now time.Time) []*EmergencyException {
	kept := s.exceptions[:0]
	for _, ex := range s.exceptions {
		if now.Before(ex.ExpiresAt) {
			kept = append(kept, ex)
		}
	}
	s.exceptions = kept
	return s.exceptions
}

// ListExceptions returns the currently active (unexpired) exceptions
// sorted by expiration, after a lazy expiry pass.
func (s *Store) ListExceptions() (int, time.Time, []EmergencyException, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	live := s.pruneExceptionsLocked(now)
	out := make([]EmergencyException, 0, len(live))
	for _, ex := range live {
		out = append(out, *ex)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ExpiresAt.Equal(out[j].ExpiresAt) {
			return out[i].ExpiresAt.Before(out[j].ExpiresAt)
		}
		return out[i].ID < out[j].ID
	})
	return s.published.Revision, now, out, nil
}
