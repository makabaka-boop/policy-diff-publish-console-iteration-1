package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"accesssim/policy"
	"accesssim/store"
)

// httpClock is an HTTP-test clock advanced by duration.
type httpClock struct {
	v atomic.Int64 // unix nanos
}

func newHTTPClock(t time.Time) *httpClock {
	c := &httpClock{}
	c.v.Store(t.UnixNano())
	return c
}
func (c *httpClock) now() time.Time { return time.Unix(0, c.v.Load()) }
func (c *httpClock) advance(d time.Duration) {
	c.v.Add(int64(d))
}

func newTestServerWithClock(t *testing.T) (*httptest.Server, *httpClock, func()) {
	t.Helper()
	clk := newHTTPClock(time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC))
	st, err := store.NewWithClock(DemoDocument(), clk.now)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	srv := NewServerWithClock(st, clk.now)
	srv.Routes(mux)
	ts := httptest.NewServer(mux)
	return ts, clk, ts.Close
}

func exceptionBody(role, resource, action string, ttl int, reason string) map[string]any {
	return map[string]any{
		"role": role, "resource": resource, "action": action,
		"ttlMinutes": ttl, "reason": reason,
	}
}

// A tuple denied by the shipped demo: base/viewer/editor are denied
// billing/read by the p20 tie (allow+deny -> tie-deny), admin escapes
// via the p100 rule.
const (
	deniedRole = "viewer"
	allowRole  = "admin"
)

func publishedEvidenceFor(t *testing.T, ts *httptest.Server, role string) map[string]any {
	t.Helper()
	r := postJSON(t, http.MethodGet, ts.URL+"/api/decisions/published", nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("decisions status = %d", r.StatusCode)
	}
	var body struct {
		Revision int `json:"revision"`
		Rows     []struct {
			Tuple struct {
				Role     string `json:"role"`
				Resource string `json:"resource"`
				Action   string `json:"action"`
			} `json:"tuple"`
			Evidence map[string]any `json:"evidence"`
		} `json:"rows"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	for _, row := range body.Rows {
		if row.Tuple.Role == role && row.Tuple.Resource == "billing" && row.Tuple.Action == "read" {
			return row.Evidence
		}
	}
	t.Fatalf("tuple (%s,billing,read) not in domain", role)
	return nil
}

// TestHTTPExceptionCreateAndEvidence is the happy path through HTTP:
// create returns 201 with coherent allow+base evidence, the published
// matrix flips, and the draft matrix and preview are untouched.
func TestHTTPExceptionCreateAndEvidence(t *testing.T) {
	ts, _, done := newTestServerWithClock(t)
	defer done()

	// Before: deny with tie-deny rule evidence, no exception fields.
	before := publishedEvidenceFor(t, ts, deniedRole)
	if before["decision"] != "deny" {
		t.Fatalf("before decision = %v, want deny", before["decision"])
	}
	if before["exception"] != nil || before["baseEvidence"] != nil {
		t.Fatalf("before evidence unexpectedly carries exception data: %v", before)
	}

	resp := postJSON(t, http.MethodPost, ts.URL+"/api/exceptions",
		exceptionBody(deniedRole, "billing", "read", 10, "drill: simulate break-glass access"))
	if resp.StatusCode != http.StatusCreated {
		buf := make([]byte, 2048)
		n, _ := resp.Body.Read(buf)
		t.Fatalf("create status = %d: %s", resp.StatusCode, buf[:n])
	}
	var created struct {
		Exception struct {
			ID                string `json:"id"`
			PublishedRevision int    `json:"publishedRevision"`
			ExpiresAt         string `json:"expiresAt"`
			Reason            string `json:"reason"`
		} `json:"exception"`
		Evidence struct {
			Decision  string         `json:"decision"`
			Reason    string         `json:"reason"`
			Exception map[string]any `json:"exception"`
			Base      struct {
				Decision string `json:"decision"`
				Reason   string `json:"reason"`
			} `json:"baseEvidence"`
			Winners []map[string]any `json:"winners"`
		} `json:"evidence"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if created.Exception.ID == "" || created.Exception.PublishedRevision != 1 {
		t.Fatalf("exception identity = %+v", created.Exception)
	}
	if created.Exception.ExpiresAt == "" || created.Exception.Reason != "drill: simulate break-glass access" {
		t.Fatalf("exception payload = %+v", created.Exception)
	}
	if created.Evidence.Decision != "allow" || created.Evidence.Reason != "emergency-exception-allow" {
		t.Fatalf("effective evidence = %+v", created.Evidence)
	}
	if created.Evidence.Base.Decision != "deny" || created.Evidence.Base.Reason != "tie-deny" {
		t.Fatalf("base evidence = %+v, want original tie-deny ruling", created.Evidence.Base)
	}
	if len(created.Evidence.Winners) != 2 {
		t.Fatalf("winners = %v, want both original tie rules preserved", created.Evidence.Winners)
	}

	// The published matrix row is now a coherent override.
	after := publishedEvidenceFor(t, ts, deniedRole)
	if after["decision"] != "allow" {
		t.Fatalf("after decision = %v, want allow", after["decision"])
	}
	exc, _ := after["exception"].(map[string]any)
	if exc == nil || exc["id"] != created.Exception.ID {
		t.Fatalf("matrix exception = %v, want id %s", after["exception"], created.Exception.ID)
	}
	base, _ := after["baseEvidence"].(map[string]any)
	if base == nil || base["decision"] != "deny" {
		t.Fatalf("matrix base evidence = %v, want preserved deny", after["baseEvidence"])
	}

	// List endpoint shows exactly one active exception at revision 1.
	lr := postJSON(t, http.MethodGet, ts.URL+"/api/exceptions", nil)
	var list struct {
		PublishedRevision int `json:"publishedRevision"`
		Exceptions        []map[string]any
	}
	json.NewDecoder(lr.Body).Decode(&list)
	lr.Body.Close()
	if list.PublishedRevision != 1 || len(list.Exceptions) != 1 {
		t.Fatalf("list = %+v, want one exception at p1", list)
	}

	// Draft matrix is unaffected: still deny, no exception fields.
	dr := postJSON(t, http.MethodGet, ts.URL+"/api/decisions/draft", nil)
	var dbody struct {
		Rows []struct {
			Tuple struct {
				Role     string `json:"role"`
				Resource string `json:"resource"`
				Action   string `json:"action"`
			} `json:"tuple"`
			Evidence map[string]any `json:"evidence"`
		} `json:"rows"`
	}
	json.NewDecoder(dr.Body).Decode(&dbody)
	dr.Body.Close()
	for _, row := range dbody.Rows {
		if row.Tuple.Role == deniedRole && row.Tuple.Resource == "billing" && row.Tuple.Action == "read" {
			if row.Evidence["decision"] != "deny" || row.Evidence["exception"] != nil {
				t.Fatalf("draft row must be pure policy: %v", row.Evidence)
			}
		}
	}

	// Preview stays pure-policy: the tuple must not show as a new allow
	// while draft == published, and existing preview/publish response
	// shapes are unchanged.
	pr := postJSON(t, http.MethodPost, ts.URL+"/api/preview", nil)
	var preview struct {
		Summary struct {
			NewAllows []any `json:"newAllows"`
		} `json:"summary"`
	}
	json.NewDecoder(pr.Body).Decode(&preview)
	pr.Body.Close()
	if len(preview.Summary.NewAllows) != 0 {
		t.Fatalf("newAllows = %v, want 0 (exceptions never enter the preview)", preview.Summary.NewAllows)
	}
}

// TestHTTPExceptionRejections covers the "whole request rejected" cases
// with their documented status codes.
func TestHTTPExceptionRejections(t *testing.T) {
	ts, _, done := newTestServerWithClock(t)
	defer done()

	cases := []struct {
		name string
		body any
		code int
		want string
	}{
		{"missing reason", exceptionBody(deniedRole, "billing", "read", 5, ""), http.StatusBadRequest, "invalid_exception_request"},
		{"ttl zero", exceptionBody(deniedRole, "billing", "read", 0, "x"), http.StatusBadRequest, "invalid_exception_request"},
		{"ttl too large", exceptionBody(deniedRole, "billing", "read", 61, "x"), http.StatusBadRequest, "invalid_exception_request"},
		{"wildcard role", exceptionBody("*", "billing", "read", 5, "x"), http.StatusBadRequest, "invalid_exception_request"},
		{"outside domain", exceptionBody(deniedRole, "billing", "frobnicate", 5, "x"), http.StatusUnprocessableEntity, "tuple_outside_domain"},
		{"currently allowed", exceptionBody(allowRole, "billing", "read", 5, "x"), http.StatusUnprocessableEntity, "tuple_not_denied"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := postJSON(t, http.MethodPost, ts.URL+"/api/exceptions", tc.body)
			if r.StatusCode != tc.code {
				t.Fatalf("status = %d, want %d", r.StatusCode, tc.code)
			}
			var eb errResp
			json.NewDecoder(r.Body).Decode(&eb)
			r.Body.Close()
			if eb.Code != tc.want {
				t.Fatalf("code = %s, want %s", eb.Code, tc.want)
			}
		})
	}

	// Nothing was created.
	lr := postJSON(t, http.MethodGet, ts.URL+"/api/exceptions", nil)
	var list struct {
		Exceptions []any `json:"exceptions"`
	}
	json.NewDecoder(lr.Body).Decode(&list)
	lr.Body.Close()
	if len(list.Exceptions) != 0 {
		t.Fatalf("exceptions after rejections = %v, want none", list.Exceptions)
	}
}

// TestHTTPExceptionDuplicateConflict: a second create for the same tuple
// returns 409 exception_already_active.
func TestHTTPExceptionDuplicateConflict(t *testing.T) {
	ts, _, done := newTestServerWithClock(t)
	defer done()

	r1 := postJSON(t, http.MethodPost, ts.URL+"/api/exceptions",
		exceptionBody(deniedRole, "billing", "read", 10, "first"))
	if r1.StatusCode != http.StatusCreated {
		t.Fatalf("first create = %d", r1.StatusCode)
	}
	r1.Body.Close()

	r2 := postJSON(t, http.MethodPost, ts.URL+"/api/exceptions",
		exceptionBody(deniedRole, "billing", "read", 10, "second"))
	if r2.StatusCode != http.StatusConflict {
		t.Fatalf("second create = %d, want 409", r2.StatusCode)
	}
	var eb errResp
	json.NewDecoder(r2.Body).Decode(&eb)
	r2.Body.Close()
	if eb.Code != "exception_already_active" {
		t.Fatalf("code = %s, want exception_already_active", eb.Code)
	}
}

// TestHTTPExceptionExpiryBoundary uses the injected clock to prove the
// tuple reverts to its original deny exactly at expiresAt with no
// background task.
func TestHTTPExceptionExpiryBoundary(t *testing.T) {
	ts, clk, done := newTestServerWithClock(t)
	defer done()

	resp := postJSON(t, http.MethodPost, ts.URL+"/api/exceptions",
		exceptionBody(deniedRole, "billing", "read", 1, "boundary test"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	resp.Body.Close()

	clk.advance(time.Minute - time.Nanosecond)
	if d := publishedEvidenceFor(t, ts, deniedRole); d["decision"] != "allow" {
		t.Fatalf("1ns before expiry decision = %v, want allow", d["decision"])
	}

	clk.advance(time.Nanosecond)
	if d := publishedEvidenceFor(t, ts, deniedRole); d["decision"] != "deny" {
		t.Fatalf("at expiry decision = %v, want deny (lazy reversion)", d["decision"])
	} else if d["exception"] != nil || d["baseEvidence"] != nil {
		t.Fatalf("expired row still carries override data: %v", d)
	}
	lr := postJSON(t, http.MethodGet, ts.URL+"/api/exceptions", nil)
	var list struct {
		Exceptions []any `json:"exceptions"`
	}
	json.NewDecoder(lr.Body).Decode(&list)
	lr.Body.Close()
	if len(list.Exceptions) != 0 {
		t.Fatalf("expired exception still listed: %v", list.Exceptions)
	}
}

// TestHTTPExceptionPublishRace interleaves create and publish against a
// real HTTP server: after publish the old exception is gone, and a late
// create can only bind to the new revision (succeeding only if it still
// denies, with new identity); publish is then possible again, proving the
// existing flow stays compatible.
func TestHTTPExceptionPublishRace(t *testing.T) {
	ts, _, done := newTestServerWithClock(t)
	defer done()

	// Exception at p1.
	r := postJSON(t, http.MethodPost, ts.URL+"/api/exceptions",
		exceptionBody(deniedRole, "billing", "read", 30, "p1 override"))
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", r.StatusCode)
	}
	var created struct {
		Exception struct {
			ID string `json:"id"`
		} `json:"exception"`
	}
	json.NewDecoder(r.Body).Decode(&created)
	r.Body.Close()

	// Publish a draft that still denies (viewer, billing, read): it only
	// touches an unrelated rule. The new revision is p2.
	resp, _ := http.Get(ts.URL + "/api/state")
	var st stateResp
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	doc := st.Draft.Document
	doc.Rules = append(doc.Rules, policy.Rule{
		ID: "race-extra", Role: "editor", Resource: "auditlog", Action: "write",
		Priority: 40, Effect: policy.EffectAllow,
	})
	sr := postJSON(t, http.MethodPut, ts.URL+"/api/draft", doc)
	sr.Body.Close()
	prev := postJSON(t, http.MethodPost, ts.URL+"/api/preview", nil)
	var p struct {
		Summary           policy.Summary `json:"summary"`
		DraftRevision     int            `json:"draftRevision"`
		PublishedRevision int            `json:"publishedRevision"`
	}
	json.NewDecoder(prev.Body).Decode(&p)
	prev.Body.Close()
	pub := postJSON(t, http.MethodPost, ts.URL+"/api/publish", map[string]any{
		"draftRevision": p.DraftRevision, "publishedRevision": p.PublishedRevision, "summary": p.Summary,
	})
	if pub.StatusCode != http.StatusOK {
		t.Fatalf("publish = %d", pub.StatusCode)
	}
	pub.Body.Close()

	// Old exception immediately gone (list revision p2, empty).
	lr := postJSON(t, http.MethodGet, ts.URL+"/api/exceptions", nil)
	var list struct {
		PublishedRevision int `json:"publishedRevision"`
		Exceptions        []map[string]any
	}
	json.NewDecoder(lr.Body).Decode(&list)
	lr.Body.Close()
	if list.PublishedRevision != 2 || len(list.Exceptions) != 0 {
		t.Fatalf("list = %+v, want empty at p2", list)
	}
	if d := publishedEvidenceFor(t, ts, deniedRole); d["decision"] != "deny" || d["exception"] != nil {
		t.Fatalf("p2 row = %v, want pure deny", d)
	}

	// Late create re-adjudicates against p2: succeeds, pinned to p2, new id.
	late := postJSON(t, http.MethodPost, ts.URL+"/api/exceptions",
		exceptionBody(deniedRole, "billing", "read", 30, "late p2 request"))
	if late.StatusCode != http.StatusCreated {
		t.Fatalf("late create = %d, want 201 (p2 still denies)", late.StatusCode)
	}
	var lc struct {
		Exception struct {
			ID                string `json:"id"`
			PublishedRevision int    `json:"publishedRevision"`
		} `json:"exception"`
	}
	json.NewDecoder(late.Body).Decode(&lc)
	late.Body.Close()
	if lc.Exception.PublishedRevision != 2 {
		t.Fatalf("late exception revision = %d, want 2", lc.Exception.PublishedRevision)
	}
	if lc.Exception.ID == created.Exception.ID {
		t.Fatal("late exception reused the p1 identity")
	}

	// Now publish a draft that ALLOWS the tuple; the p2 exception is
	// cleared again and another late create is refused by p3.
	resp2, _ := http.Get(ts.URL + "/api/state")
	var st2 stateResp
	json.NewDecoder(resp2.Body).Decode(&st2)
	resp2.Body.Close()
	doc2 := st2.Draft.Document
	doc2.Rules = append(doc2.Rules, policy.Rule{
		ID: "viewer-billing-allow", Role: "viewer", Resource: "billing", Action: "read",
		Priority: 200, Effect: policy.EffectAllow,
	})
	sr2 := postJSON(t, http.MethodPut, ts.URL+"/api/draft", doc2)
	sr2.Body.Close()
	prev2 := postJSON(t, http.MethodPost, ts.URL+"/api/preview", nil)
	var p2 struct {
		Summary           policy.Summary `json:"summary"`
		DraftRevision     int            `json:"draftRevision"`
		PublishedRevision int            `json:"publishedRevision"`
	}
	json.NewDecoder(prev2.Body).Decode(&p2)
	prev2.Body.Close()
	pub2 := postJSON(t, http.MethodPost, ts.URL+"/api/publish", map[string]any{
		"draftRevision": p2.DraftRevision, "publishedRevision": p2.PublishedRevision, "summary": p2.Summary,
	})
	if pub2.StatusCode != http.StatusOK {
		t.Fatalf("second publish = %d", pub2.StatusCode)
	}
	pub2.Body.Close()
	if d := publishedEvidenceFor(t, ts, deniedRole); d["decision"] != "allow" || d["exception"] != nil {
		t.Fatalf("p3 row = %v, want pure rule allow with no exception", d)
	}
	refused := postJSON(t, http.MethodPost, ts.URL+"/api/exceptions",
		exceptionBody(deniedRole, "billing", "read", 30, "late against p3"))
	if refused.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("late create against allowing p3 = %d, want 422", refused.StatusCode)
	}
	refused.Body.Close()
}

// TestHTTPExceptionResetInvalidates: reset swaps in fresh data and all
// exceptions disappear immediately.
func TestHTTPExceptionResetInvalidates(t *testing.T) {
	ts, _, done := newTestServerWithClock(t)
	defer done()

	r := postJSON(t, http.MethodPost, ts.URL+"/api/exceptions",
		exceptionBody(deniedRole, "billing", "read", 60, "before reset"))
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", r.StatusCode)
	}
	r.Body.Close()

	reset := postJSON(t, http.MethodPost, ts.URL+"/api/demo/reset", nil)
	if reset.StatusCode != http.StatusOK {
		t.Fatalf("reset = %d", reset.StatusCode)
	}
	reset.Body.Close()

	lr := postJSON(t, http.MethodGet, ts.URL+"/api/exceptions", nil)
	var list struct {
		PublishedRevision int `json:"publishedRevision"`
		Exceptions        []any
	}
	json.NewDecoder(lr.Body).Decode(&list)
	lr.Body.Close()
	if list.PublishedRevision != 1 || len(list.Exceptions) != 0 {
		t.Fatalf("after reset = %+v, want empty at p1", list)
	}
	if d := publishedEvidenceFor(t, ts, deniedRole); d["decision"] != "deny" || d["exception"] != nil {
		t.Fatalf("row after reset = %v, want pure deny", d)
	}

	// Server keeps using the injected clock on the reset store: a new
	// exception still works there.
	again := postJSON(t, http.MethodPost, ts.URL+"/api/exceptions",
		exceptionBody(deniedRole, "billing", "read", 60, "after reset"))
	if again.StatusCode != http.StatusCreated {
		t.Fatalf("create after reset = %d, want 201", again.StatusCode)
	}
	again.Body.Close()
}

// TestHTTPExceptionMalformedJSON confirms body decoding errors stay 400.
func TestHTTPExceptionMalformedJSON(t *testing.T) {
	ts, _, done := newTestServerWithClock(t)
	defer done()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/exceptions", bytes.NewBufferString("{not json"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed body = %d, want 400", resp.StatusCode)
	}
}
