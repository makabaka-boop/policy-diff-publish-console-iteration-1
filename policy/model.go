// Package policy defines the finite-domain access policy model.
//
// This is a SIMULATION/TEACHING tool: it computes decisions over an
// explicitly enumerated, finite set of (role, resource class, action)
// tuples. It is not an authorization entry point for any real system.
package policy

import (
	"fmt"
	"regexp"
	"sort"
)

// Hard caps of the simulated finite domain.
const (
	MaxRoles     = 8
	MaxResources = 8
	MaxActions   = 6
	MaxRules     = 200
	MaxPriority  = 1_000_000

	// Wildcard matches any concrete role / resource / action.
	Wildcard = "*"
)

// Effects.
const (
	EffectAllow = "allow"
	EffectDeny  = "deny"
)

var nameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// Role is a node in the (acyclic) role inheritance DAG.
type Role struct {
	Name    string   `json:"name"`
	Parents []string `json:"parents"`
}

// Rule matches a subject role (or one of its ancestors), a resource
// class and an action. Any of the three selectors may be "*".
type Rule struct {
	ID       string `json:"id"`
	Role     string `json:"role"`
	Resource string `json:"resource"`
	Action   string `json:"action"`
	Priority int    `json:"priority"`
	Effect   string `json:"effect"`
}

// Document is the full editable policy: roles with their inheritance
// edges, the finite resource/action universe, and the rule set.
type Document struct {
	Roles     []Role   `json:"roles"`
	Resources []string `json:"resources"`
	Actions   []string `json:"actions"`
	Rules     []Rule   `json:"rules"`
}

// Validate checks the caps, name rules, referential integrity, unique
// rule ids and the acyclic inheritance constraint. Validation is
// deliberately order-independent; slices are sorted by the engine.
func (d *Document) Validate() error {
	if len(d.Roles) == 0 {
		return fmt.Errorf("at least one role is required")
	}
	if len(d.Roles) > MaxRoles {
		return fmt.Errorf("at most %d roles allowed, got %d", MaxRoles, len(d.Roles))
	}
	if len(d.Resources) == 0 {
		return fmt.Errorf("at least one resource class is required")
	}
	if len(d.Resources) > MaxResources {
		return fmt.Errorf("at most %d resource classes allowed, got %d", MaxResources, len(d.Resources))
	}
	if len(d.Actions) == 0 {
		return fmt.Errorf("at least one action is required")
	}
	if len(d.Actions) > MaxActions {
		return fmt.Errorf("at most %d actions allowed, got %d", MaxActions, len(d.Actions))
	}
	if len(d.Rules) > MaxRules {
		return fmt.Errorf("at most %d rules allowed, got %d", MaxRules, len(d.Rules))
	}

	roleSet := map[string]bool{}
	for _, r := range d.Roles {
		if !nameRE.MatchString(r.Name) {
			return fmt.Errorf("invalid role name %q: 1-32 chars of [A-Za-z0-9_-]", r.Name)
		}
		if roleSet[r.Name] {
			return fmt.Errorf("duplicate role name %q", r.Name)
		}
		roleSet[r.Name] = true
	}
	// Parents are validated in a second pass so order in the slice does not matter.
	for _, r := range d.Roles {
		seen := map[string]bool{}
		for _, p := range r.Parents {
			if !roleSet[p] {
				return fmt.Errorf("role %q inherits from unknown role %q", r.Name, p)
			}
			if p == r.Name {
				return fmt.Errorf("role %q cannot inherit from itself", r.Name)
			}
			if seen[p] {
				return fmt.Errorf("role %q lists parent %q more than once", r.Name, p)
			}
			seen[p] = true
		}
	}
	if err := checkAcyclic(d.Roles); err != nil {
		return err
	}

	resSet := map[string]bool{}
	for _, r := range d.Resources {
		if !nameRE.MatchString(r) {
			return fmt.Errorf("invalid resource name %q", r)
		}
		if r == Wildcard {
			return fmt.Errorf("resource name %q is reserved", Wildcard)
		}
		if resSet[r] {
			return fmt.Errorf("duplicate resource name %q", r)
		}
		resSet[r] = true
	}
	actSet := map[string]bool{}
	for _, a := range d.Actions {
		if !nameRE.MatchString(a) {
			return fmt.Errorf("invalid action name %q", a)
		}
		if a == Wildcard {
			return fmt.Errorf("action name %q is reserved", Wildcard)
		}
		if actSet[a] {
			return fmt.Errorf("duplicate action name %q", a)
		}
		actSet[a] = true
	}

	ids := map[string]bool{}
	for i, rl := range d.Rules {
		if !nameRE.MatchString(rl.ID) {
			return fmt.Errorf("rule #%d: invalid id %q", i+1, rl.ID)
		}
		if ids[rl.ID] {
			return fmt.Errorf("duplicate rule id %q", rl.ID)
		}
		ids[rl.ID] = true
		if rl.Role != Wildcard && !roleSet[rl.Role] {
			return fmt.Errorf("rule %q references unknown role %q", rl.ID, rl.Role)
		}
		if rl.Resource != Wildcard && !resSet[rl.Resource] {
			return fmt.Errorf("rule %q references unknown resource %q", rl.ID, rl.Resource)
		}
		if rl.Action != Wildcard && !actSet[rl.Action] {
			return fmt.Errorf("rule %q references unknown action %q", rl.ID, rl.Action)
		}
		if rl.Priority < 0 || rl.Priority > MaxPriority {
			return fmt.Errorf("rule %q: priority must be within [0,%d]", rl.ID, MaxPriority)
		}
		if rl.Effect != EffectAllow && rl.Effect != EffectDeny {
			return fmt.Errorf("rule %q: effect must be %q or %q", rl.ID, EffectAllow, EffectDeny)
		}
	}
	return nil
}

// checkAcyclic runs an iterative DFS with white/grey/black coloring over
// the parent edges. Edge parent -> child would also do; cycle detection
// is identical either way.
func checkAcyclic(roles []Role) error {
	parents := make(map[string][]string, len(roles))
	for _, r := range roles {
		parents[r.Name] = append([]string(nil), r.Parents...)
	}
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := map[string]int{}
	for _, r := range roles {
		color[r.Name] = white
	}

	// Frame records the node and the next parent edge to visit.
	type frame struct {
		role  string
		index int
	}
	names := make([]string, 0, len(roles))
	for _, r := range roles {
		names = append(names, r.Name)
	}
	sort.Strings(names)

	for _, start := range names {
		if color[start] != white {
			continue
		}
		color[start] = grey
		stack := []frame{{role: start}}
		for len(stack) > 0 {
			top := &stack[len(stack)-1]
			ps := parents[top.role]
			if top.index >= len(ps) {
				color[top.role] = black
				stack = stack[:len(stack)-1]
				continue
			}
			next := ps[top.index]
			top.index++
			switch color[next] {
			case grey:
				return fmt.Errorf("role inheritance graph contains a cycle involving %q", next)
			case white:
				color[next] = grey
				stack = append(stack, frame{role: next})
			}
		}
	}
	return nil
}

// Clone returns a deep copy so callers cannot mutate a validated document
// through shared slice backing arrays.
func (d *Document) Clone() *Document {
	cp := &Document{
		Resources: append([]string(nil), d.Resources...),
		Actions:   append([]string(nil), d.Actions...),
		Roles:     make([]Role, len(d.Roles)),
		Rules:     make([]Rule, len(d.Rules)),
	}
	for i, r := range d.Roles {
		cp.Roles[i] = Role{Name: r.Name, Parents: append([]string(nil), r.Parents...)}
	}
	copy(cp.Rules, d.Rules)
	return cp
}
