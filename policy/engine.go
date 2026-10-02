package policy

import (
	"sort"
)

// RuleRef is evidence that a rule participated in (or won) a decision.
type RuleRef struct {
	RuleID   string `json:"ruleId"`
	Priority int    `json:"priority"`
	Effect   string `json:"effect"`
}

// Evidence explains exactly how a tuple decision was reached.
type Evidence struct {
	// Decision is the resulting effect ("allow" or "deny").
	Decision string `json:"decision"`
	// Reason is "single-winner", "tie-deny" or "no-match-default-deny".
	Reason string `json:"reason"`
	// Considered lists every rule that matched the tuple, highest
	// priority first. On a tie the winners group is also exposed.
	Considered []RuleRef `json:"considered"`
	// Winners are the rules at the decisive (highest matched) priority.
	Winners []RuleRef `json:"winners"`
}

// Tuple is one point of the finite decision domain.
type Tuple struct {
	Role     string `json:"role"`
	Resource string `json:"resource"`
	Action   string `json:"action"`
}

const (
	ReasonSingleWinner = "single-winner"
	ReasonTieDeny      = "tie-deny"
	ReasonNoMatch      = "no-match-default-deny"
)

// Engine holds precomputed indexes for one validated document.
type Engine struct {
	doc *Document

	roles     []string
	resources []string
	actions   []string

	// ancestors[r] includes r itself plus every inherited role.
	ancestors map[string]map[string]bool
	rules     []Rule
}

// NewEngine validates the document and builds lookup indexes.
func NewEngine(doc *Document) (*Engine, error) {
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	e := &Engine{
		doc:       doc.Clone(),
		ancestors: map[string]map[string]bool{},
	}

	e.roles = make([]string, 0, len(doc.Roles))
	for _, r := range doc.Roles {
		e.roles = append(e.roles, r.Name)
	}
	sort.Strings(e.roles)
	e.resources = append([]string(nil), doc.Resources...)
	sort.Strings(e.resources)
	e.actions = append([]string(nil), doc.Actions...)
	sort.Strings(e.actions)

	// Build ancestor sets via repeated expansion along parent edges.
	// With <=8 roles and an acyclic graph this simple fixpoint is fine
	// and easy to audit.
	direct := map[string][]string{}
	for _, r := range doc.Roles {
		direct[r.Name] = append([]string(nil), r.Parents...)
	}
	for _, name := range e.roles {
		set := map[string]bool{name: true}
		frontier := append([]string(nil), direct[name]...)
		for len(frontier) > 0 {
			cur := frontier[len(frontier)-1]
			frontier = frontier[:len(frontier)-1]
			if set[cur] {
				continue
			}
			set[cur] = true
			frontier = append(frontier, direct[cur]...)
		}
		e.ancestors[name] = set
	}

	e.rules = append([]Rule(nil), doc.Rules...)
	return e, nil
}

// Roles returns the sorted concrete role names.
func (e *Engine) Roles() []string { return append([]string(nil), e.roles...) }

// Document returns a deep copy of the engine's validated document.
func (e *Engine) Document() *Document { return e.doc.Clone() }

// matches reports whether rule rl applies to tuple t for subject role role:
// the rule selector names the role itself or any ancestor role it inherits.
func (e *Engine) matches(rl Rule, t Tuple) bool {
	if rl.Role != Wildcard && !e.ancestors[t.Role][rl.Role] {
		return false
	}
	if rl.Resource != Wildcard && rl.Resource != t.Resource {
		return false
	}
	if rl.Action != Wildcard && rl.Action != t.Action {
		return false
	}
	return true
}

// Decide evaluates one concrete tuple independently.
//
// Semantics:
//   - collect all matching rules;
//   - only the highest matched priority group matters;
//   - one rule, or several rules that all agree -> that effect;
//   - allow vs deny at the same priority -> deny wins ("tie-deny");
//   - no rule matches -> default deny.
func (e *Engine) Decide(t Tuple) Evidence {
	matched := make([]RuleRef, 0)
	topPriority := -1
	for _, rl := range e.rules {
		if !e.matches(rl, t) {
			continue
		}
		matched = append(matched, RuleRef{
			RuleID:   rl.ID,
			Priority: rl.Priority,
			Effect:   rl.Effect,
		})
		if rl.Priority > topPriority {
			topPriority = rl.Priority
		}
	}
	// Deterministic evidence order: priority desc, then rule id.
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].Priority != matched[j].Priority {
			return matched[i].Priority > matched[j].Priority
		}
		return matched[i].RuleID < matched[j].RuleID
	})

	if len(matched) == 0 {
		return Evidence{
			Decision:   EffectDeny,
			Reason:     ReasonNoMatch,
			Considered: []RuleRef{},
			Winners:    []RuleRef{},
		}
	}

	winners := make([]RuleRef, 0)
	for _, m := range matched {
		if m.Priority != topPriority {
			break
		}
		winners = append(winners, m)
	}

	deny := false
	allow := false
	for _, w := range winners {
		switch w.Effect {
		case EffectDeny:
			deny = true
		case EffectAllow:
			allow = true
		}
	}

	ev := Evidence{
		Decision:   EffectAllow,
		Reason:     ReasonSingleWinner,
		Considered: matched,
		Winners:    winners,
	}
	switch {
	case deny && allow:
		ev.Decision = EffectDeny
		ev.Reason = ReasonTieDeny
	case deny:
		ev.Decision = EffectDeny
	default:
		ev.Decision = EffectAllow
	}
	return ev
}

// Domain enumerates every (role, resource, action) tuple in a stable
// order: role, then resource, then action, each ascending.
func (e *Engine) Domain() []Tuple {
	out := make([]Tuple, 0, len(e.roles)*len(e.resources)*len(e.actions))
	for _, r := range e.roles {
		for _, res := range e.resources {
			for _, a := range e.actions {
				out = append(out, Tuple{Role: r, Resource: res, Action: a})
			}
		}
	}
	return out
}

// Decisions evaluates the whole finite domain.
func (e *Engine) Decisions() map[Tuple]Evidence {
	out := make(map[Tuple]Evidence, len(e.roles)*len(e.resources)*len(e.actions))
	for _, t := range e.Domain() {
		out[t] = e.Decide(t)
	}
	return out
}
