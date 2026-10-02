package api

import "accesssim/policy"

// DemoDocument is the seed policy. Its roles form the classic diamond
// where the privileged role is the JOIN (it inherits both middle roles):
//
//	      base
//	     /    \
//	viewer    editor
//	     \    /
//	     admin
//
// Thus a rule on "admin" applies only to admin, while rules on viewer /
// editor propagate to admin along the two inheritance paths.
//
// The rules exercise every decision path: wildcard role/resource/action,
// inherited-role matching, same-priority allow/deny conflict (deny
// wins), higher-priority overrides both ways, and default deny.
func DemoDocument() *policy.Document {
	return &policy.Document{
		Roles: []policy.Role{
			{Name: "base", Parents: nil},
			{Name: "viewer", Parents: []string{"base"}},
			{Name: "editor", Parents: []string{"base"}},
			{Name: "admin", Parents: []string{"editor", "viewer"}},
		},
		Resources: []string{"doc", "billing", "auditlog"},
		Actions:   []string{"read", "write", "delete"},
		Rules: []policy.Rule{
			// Baseline: nothing matches anything except default deny;
			// the explicit wildcard deny documents intent and provides
			// a low-priority considered-rule for evidence displays.
			{ID: "r-deny-all", Role: "*", Resource: "*", Action: "*", Priority: 0, Effect: policy.EffectDeny},

			// viewer (and admin inheriting viewer) may read docs.
			{ID: "r-viewer-read-doc", Role: "viewer", Resource: "doc", Action: "read", Priority: 10, Effect: policy.EffectAllow},

			// editor (and admin) may write docs.
			{ID: "r-editor-write-doc", Role: "editor", Resource: "doc", Action: "write", Priority: 10, Effect: policy.EffectAllow},

			// Same-priority conflict: an allow and a deny both match
			// billing/read at priority 20. Deny must win the tie for
			// base/viewer/editor; admin escapes via the higher-priority
			// rule below, which also demonstrates priority override.
			{ID: "r-billing-read-allow", Role: "viewer", Resource: "billing", Action: "read", Priority: 20, Effect: policy.EffectAllow},
			{ID: "r-billing-read-deny", Role: "*", Resource: "billing", Action: "read", Priority: 20, Effect: policy.EffectDeny},

			// Deletes are denied even where a lower rule would allow;
			// admin's priority-100 rule overrides this for admin only.
			{ID: "r-no-delete", Role: "*", Resource: "*", Action: "delete", Priority: 30, Effect: policy.EffectDeny},
			{ID: "r-admin-all", Role: "admin", Resource: "*", Action: "*", Priority: 100, Effect: policy.EffectAllow},
		},
	}
}
