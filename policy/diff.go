package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// DiffEntry records one tuple whose decision flipped between the
// published version and the draft, with the winning evidence on both
// sides so the UI can show "why".
type DiffEntry struct {
	Tuple  Tuple    `json:"tuple"`
	Before Evidence `json:"before"`
	After  Evidence `json:"after"`
	FromTo string   `json:"fromTo"` // "deny->allow" | "allow->deny"
}

// Summary is the exhaustive comparison of one draft revision against
// the published revision it was previewed over. It is the artifact a
// client must present (verbatim, by hash) when publishing.
type Summary struct {
	DraftRevision     int         `json:"draftRevision"`
	PublishedRevision int         `json:"publishedRevision"`
	TupleCount        int         `json:"tupleCount"`
	AllowCount        int         `json:"allowCount"`
	DenyCount         int         `json:"denyCount"`
	NewAllows         []DiffEntry `json:"newAllows"`
	NewDenies         []DiffEntry `json:"newDenies"`
	Unchanged         int         `json:"unchanged"`
	// Hash binds the previewed drafts/published revision pair to the
	// full enumerated result, so a tampered or stale summary cannot be
	// published. It is filled in by BuildSummary.
	Hash string `json:"hash"`
}

func noMatchEvidence() Evidence {
	return Evidence{
		Decision:   EffectDeny,
		Reason:     ReasonNoMatch,
		Considered: []RuleRef{},
		Winners:    []RuleRef{},
	}
}

// BuildSummary evaluates the union finite domain of both documents and
// lists every decision flip. Tuples outside a version's concrete domain
// are treated as default-deny on that side.
func BuildSummary(draft *Engine, draftRev int, published *Engine, publishedRev int) (*Summary, error) {
	roleSet := map[string]bool{}
	resSet := map[string]bool{}
	actSet := map[string]bool{}
	for _, r := range append(draft.roles, published.roles...) {
		roleSet[r] = true
	}
	for _, r := range append(draft.resources, published.resources...) {
		resSet[r] = true
	}
	for _, a := range append(draft.actions, published.actions...) {
		actSet[a] = true
	}
	roles := sortedKeys(roleSet)
	resources := sortedKeys(resSet)
	actions := sortedKeys(actSet)

	s := &Summary{
		DraftRevision:     draftRev,
		PublishedRevision: publishedRev,
		NewAllows:         []DiffEntry{},
		NewDenies:         []DiffEntry{},
	}

	eval := func(eng *Engine, t Tuple) (Evidence, bool) {
		if contains(eng.roles, t.Role) && contains(eng.resources, t.Resource) && contains(eng.actions, t.Action) {
			return eng.Decide(t), true
		}
		return noMatchEvidence(), false
	}

	for _, role := range roles {
		for _, res := range resources {
			for _, act := range actions {
				t := Tuple{Role: role, Resource: res, Action: act}
				before, _ := eval(published, t)
				after, inDraft := eval(draft, t)
				// Only tuples that actually exist in the draft count as
				// part of its decision domain; tuples removed entirely
				// still surface as deny flips but not in the totals.
				if inDraft {
					s.TupleCount++
					if after.Decision == EffectAllow {
						s.AllowCount++
					} else {
						s.DenyCount++
					}
				}
				switch {
				case before.Decision == EffectDeny && after.Decision == EffectAllow:
					s.NewAllows = append(s.NewAllows, DiffEntry{
						Tuple: t, Before: before, After: after, FromTo: "deny->allow",
					})
				case before.Decision == EffectAllow && after.Decision == EffectDeny:
					s.NewDenies = append(s.NewDenies, DiffEntry{
						Tuple: t, Before: before, After: after, FromTo: "allow->deny",
					})
				default:
					s.Unchanged++
				}
			}
		}
	}

	hash, err := s.computeHash()
	if err != nil {
		return nil, err
	}
	s.Hash = hash
	return s, nil
}

// canonicalSummary is the exact subset that the integrity hash covers.
// The hash is deliberately not included inside itself.
type canonicalSummary struct {
	DraftRevision     int         `json:"draftRevision"`
	PublishedRevision int         `json:"publishedRevision"`
	TupleCount        int         `json:"tupleCount"`
	AllowCount        int         `json:"allowCount"`
	DenyCount         int         `json:"denyCount"`
	NewAllows         []DiffEntry `json:"newAllows"`
	NewDenies         []DiffEntry `json:"newDenies"`
	Unchanged         int         `json:"unchanged"`
}

func (s *Summary) computeHash() (string, error) {
	c := canonicalSummary{
		DraftRevision:     s.DraftRevision,
		PublishedRevision: s.PublishedRevision,
		TupleCount:        s.TupleCount,
		AllowCount:        s.AllowCount,
		DenyCount:         s.DenyCount,
		NewAllows:         s.NewAllows,
		NewDenies:         s.NewDenies,
		Unchanged:         s.Unchanged,
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// VerifyHash recomputes the hash and reports whether it matches.
func (s *Summary) VerifyHash() (bool, error) {
	want, err := s.computeHash()
	if err != nil {
		return false, err
	}
	return want == s.Hash, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(sorted []string, v string) bool {
	i := sort.SearchStrings(sorted, v)
	return i < len(sorted) && sorted[i] == v
}
