package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"accesssim/policy"
	"accesssim/store"
)

func jsonReader(v any) *bytes.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

func newHTTPTS(t *testing.T, mux *http.ServeMux) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// clockSrv builds a server whose store uses a controllable clock.
func clockSrv(t *testing.T, start time.Time) (*Server, *testClock) {
	t.Helper()
	c := &testClock{t: start}
	st, err := store.NewWithClock(DemoDocument(), c.now)
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(st), c
}

type testClock struct{ t time.Time }

func (c *testClock) now() time.Time      { return c.t }
func (c *testClock) add(d time.Duration) { c.t = c.t.Add(d) }

type exceptionPayload struct {
	ID                string    `json:"id"`
	Tuple             tupleJSON `json:"tuple"`
	Reason            string    `json:"reason"`
	CreatedAt         time.Time `json:"createdAt"`
	ExpiresAt         time.Time `json:"expiresAt"`
	PublishedRevision int       `json:"publishedRevision"`
}

type tupleJSON struct {
	Role     string `json:"role"`
	Resource string `json:"resource"`
	Action   string `json:"action"`
}

type decisionsPayload struct {
	Version  string `json:"version"`
	Revision int    `json:"revision"`
	Rows     []struct {
		Tuple     tupleJSON         `json:"tuple"`
		Evidence  policy.Evidence   `json:"evidence"`
		Exception *exceptionPayload `json:"exception,omitempty"`
	} `json:"rows"`
}

func mustDecisions(t *testing.T, base, version string) decisionsPayload {
	t.Helper()
	resp, err := http.Get(base + "/api/decisions/" + version)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("decisions/%s status %d", version, resp.StatusCode)
	}
	var d decisionsPayload
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	return d
}

func findDecision(d decisionsPayload, role, resource, action string) (int, bool) {
	for i, r := range d.Rows {
		if r.Tuple.Role == role && r.Tuple.Resource == resource && r.Tuple.Action == action {
			return i, true
		}
	}
	return 0, false
}

func createException(t *testing.T, base string, body any) *http.Response {
	t.Helper()
	return postJSON(t, http.MethodPost, base+"/api/exceptions", body)
}

// TestHTTPExceptionLifecycle verifies the end-to-end flow through real
// HTTP: re-adjudicated create (201) returns one coherent payload
// (allow + exception id + original deny evidence), the published matrix
// shows the temporary allow, the draft matrix stays denied, and the list
// endpoint reports the exception with reason and expiry.
func TestHTTPExceptionLifecycle(t *testing.T) {
	mux := http.NewServeMux()
	srv, c := clockSrv(t, time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC))
	srv.Routes(mux)
	ts := newHTTPTS(t, mux)
	defer ts.Close()

	// viewer/billing/read is a same-priority tie -> deny on published p1.
	const role, resource, action = "viewer", "billing", "read"
	pub := mustDecisions(t, ts.URL, "published")
	i, ok := findDecision(pub, role, resource, action)
	if !ok {
		t.Fatal("tuple missing from published domain")
	}
	if pub.Rows[i].Evidence.Decision != "deny" ||
		pub.Rows[i].Evidence.Reason != policy.ReasonTieDeny {
		t.Fatalf("precondition evidence = %+v, want tie-deny", pub.Rows[i].Evidence)
	}

	resp := createException(t, ts.URL, map[string]any{
		"tuple":             map[string]string{"role": role, "resource": resource, "action": action},
		"reason":            "演练：临时让 viewer 查看账单只读 3 分钟",
		"ttlMinutes":        3,
		"publishedRevision": 1,
	})
	if resp.StatusCode != http.StatusCreated {
		buf := make([]byte, 2048)
		n, _ := resp.Body.Read(buf)
		t.Fatalf("create status = %d: %s", resp.StatusCode, buf[:n])
	}
	var created struct {
		Exception exceptionPayload `json:"exception"`
		Decision  struct {
			Tuple     tupleJSON         `json:"tuple"`
			Evidence  policy.Evidence   `json:"evidence"`
			Exception *exceptionPayload `json:"exception"`
		} `json:"decision"`
		Now string `json:"now"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	ex := created.Exception
	if ex.ID == "" || ex.PublishedRevision != 1 || ex.Reason == "" {
		t.Fatalf("created exception = %+v", ex)
	}
	if !ex.ExpiresAt.Equal(c.t.Add(3 * time.Minute)) {
		t.Fatalf("expiry = %v", ex.ExpiresAt)
	}
	ev := created.Decision.Evidence
	if ev.Decision != "allow" || ev.Reason != policy.ReasonEmergencyAllow ||
		ev.ExceptionID != ex.ID || ev.OriginalReason != policy.ReasonTieDeny {
		t.Fatalf("created decision evidence = %+v", ev)
	}
	if len(ev.Winners) != 2 {
		t.Fatalf("original tie winners = %v, want both p20 rules preserved", ev.Winners)
	}
	if created.Decision.Exception == nil {
		t.Fatal("create payload mixes allow without exception object")
	}

	// Published matrix reflects the override, coherently.
	pub = mustDecisions(t, ts.URL, "published")
	i, _ = findDecision(pub, role, resource, action)
	if pub.Rows[i].Evidence.Decision != "allow" ||
		pub.Rows[i].Evidence.ExceptionID != ex.ID ||
		pub.Rows[i].Exception == nil ||
		pub.Rows[i].Exception.ID != ex.ID {
		t.Fatalf("published row after create = %+v", pub.Rows[i])
	}

	// Draft matrix remains rule-only deny.
	draft := mustDecisions(t, ts.URL, "draft")
	di, _ := findDecision(draft, role, resource, action)
	if draft.Rows[di].Evidence.Decision != "deny" ||
		draft.Rows[di].Evidence.ExceptionID != "" ||
		draft.Rows[di].Exception != nil {
		t.Fatalf("draft row = %+v, exceptions must not touch draft", draft.Rows[di])
	}

	// List endpoint.
	lr, err := http.Get(ts.URL + "/api/exceptions")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Exceptions        []exceptionPayload `json:"exceptions"`
		PublishedRevision int                `json:"publishedRevision"`
		Now               string             `json:"now"`
	}
	json.NewDecoder(lr.Body).Decode(&list)
	lr.Body.Close()
	if list.PublishedRevision != 1 || len(list.Exceptions) != 1 ||
		list.Exceptions[0].ID != ex.ID {
		t.Fatalf("list = %+v", list)
	}
}

// TestHTTPExceptionRejections covers invalid tuples, TTL/reason bounds
// and the "currently allowed" rule — each rejected as a whole request.
func TestHTTPExceptionRejections(t *testing.T) {
	mux := http.NewServeMux()
	srv, _ := clockSrv(t, time.Now())
	srv.Routes(mux)
	ts := newHTTPTS(t, mux)
	defer ts.Close()

	cases := []struct {
		name string
		body map[string]any
		want int
		code string
	}{
		{"ttl zero", map[string]any{
			"tuple":  map[string]string{"role": "base", "resource": "doc", "action": "read"},
			"reason": "x", "ttlMinutes": 0, "publishedRevision": 1},
			422, "invalid_exception"},
		{"ttl 61", map[string]any{
			"tuple":  map[string]string{"role": "base", "resource": "doc", "action": "read"},
			"reason": "x", "ttlMinutes": 61, "publishedRevision": 1},
			422, "invalid_exception"},
		{"no reason", map[string]any{
			"tuple":  map[string]string{"role": "base", "resource": "doc", "action": "read"},
			"reason": "  ", "ttlMinutes": 5, "publishedRevision": 1},
			422, "invalid_exception"},
		{"foreign tuple", map[string]any{
			"tuple":  map[string]string{"role": "nobody", "resource": "doc", "action": "read"},
			"reason": "x", "ttlMinutes": 5, "publishedRevision": 1},
			422, "invalid_exception"},
		{"missing revision", map[string]any{
			"tuple":  map[string]string{"role": "base", "resource": "doc", "action": "read"},
			"reason": "x", "ttlMinutes": 5},
			422, "invalid_exception"},
		{"tuple currently allowed", map[string]any{
			"tuple":  map[string]string{"role": "admin", "resource": "doc", "action": "delete"},
			"reason": "x", "ttlMinutes": 5, "publishedRevision": 1},
			409, "tuple_not_denied"},
		{"stale revision on create", map[string]any{
			"tuple":  map[string]string{"role": "base", "resource": "doc", "action": "read"},
			"reason": "x", "ttlMinutes": 5, "publishedRevision": 42},
			409, "published_moved"},
		{"duplicate", nil, // filled below after the first create
			409, "exception_exists"},
	}

	// Seed the duplicate case.
	first := createException(t, ts.URL, map[string]any{
		"tuple":             map[string]string{"role": "base", "resource": "doc", "action": "read"},
		"reason":            "drill",
		"ttlMinutes":        10,
		"publishedRevision": 1,
	})
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("seed create status %d", first.StatusCode)
	}
	first.Body.Close()
	cases[len(cases)-1].body = map[string]any{
		"tuple":             map[string]string{"role": "base", "resource": "doc", "action": "read"},
		"reason":            "drill2",
		"ttlMinutes":        10,
		"publishedRevision": 1,
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := createException(t, ts.URL, tc.body)
			defer r.Body.Close()
			if r.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", r.StatusCode, tc.want)
			}
			var e errResp
			json.NewDecoder(r.Body).Decode(&e)
			if e.Code != tc.code {
				t.Fatalf("code = %q, want %q", e.Code, tc.code)
			}
		})
	}
}

// TestHTTPExceptionExpiry drives the injected clock over HTTP: after the
// TTL elapses the next read restores the original deny with no exception
// traces — no background task involved.
func TestHTTPExceptionExpiry(t *testing.T) {
	mux := http.NewServeMux()
	srv, c := clockSrv(t, time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC))
	srv.Routes(mux)
	ts := newHTTPTS(t, mux)
	defer ts.Close()

	r := createException(t, ts.URL, map[string]any{
		"tuple":             map[string]string{"role": "base", "resource": "doc", "action": "write"},
		"reason":            "expiry drill",
		"ttlMinutes":        1,
		"publishedRevision": 1,
	})
	if r.StatusCode != 201 {
		t.Fatalf("create status %d", r.StatusCode)
	}
	r.Body.Close()

	c.add(time.Minute - time.Second)
	d := mustDecisions(t, ts.URL, "published")
	i, _ := findDecision(d, "base", "doc", "write")
	if d.Rows[i].Evidence.Decision != "allow" {
		t.Fatal("still should be overridden 1s before expiry")
	}

	c.add(time.Second) // exactly at expiry
	d = mustDecisions(t, ts.URL, "published")
	i, _ = findDecision(d, "base", "doc", "write")
	if d.Rows[i].Evidence.Decision != "deny" ||
		d.Rows[i].Evidence.ExceptionID != "" || d.Rows[i].Exception != nil {
		t.Fatalf("post-expiry row = %+v, want plain deny", d.Rows[i])
	}
	lr, _ := http.Get(ts.URL + "/api/exceptions")
	var list struct {
		Exceptions []exceptionPayload `json:"exceptions"`
	}
	json.NewDecoder(lr.Body).Decode(&list)
	lr.Body.Close()
	if len(list.Exceptions) != 0 {
		t.Fatalf("expired exceptions still listed: %+v", list.Exceptions)
	}
}

// TestHTTPExceptionPublishAndResetInvalidate: publishing a new revision
// and resetting demo data each clear all exceptions immediately; a late
// request stamped with the old revision cannot attach to the new one.
func TestHTTPExceptionPublishAndResetInvalidate(t *testing.T) {
	mux := http.NewServeMux()
	srv, _ := clockSrv(t, time.Now())
	srv.Routes(mux)
	ts := newHTTPTS(t, mux)
	defer ts.Close()

	// Exception at p1.
	r := createException(t, ts.URL, map[string]any{
		"tuple":             map[string]string{"role": "base", "resource": "doc", "action": "read"},
		"reason":            "pre-publish drill",
		"ttlMinutes":        30,
		"publishedRevision": 1,
	})
	if r.StatusCode != 201 {
		t.Fatalf("create status %d", r.StatusCode)
	}
	r.Body.Close()

	// Publish a changed draft -> p2.
	resp, _ := http.Get(ts.URL + "/api/state")
	var st stateResp
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	doc := st.Draft.Document
	doc.Rules = append(doc.Rules, policy.Rule{
		ID: "http-pub-x", Role: "viewer", Resource: "auditlog", Action: "delete",
		Priority: 40, Effect: policy.EffectAllow,
	})
	sr := postJSON(t, http.MethodPut, ts.URL+"/api/draft", doc)
	sr.Body.Close()
	pr := postJSON(t, http.MethodPost, ts.URL+"/api/preview", nil)
	var prev struct {
		Summary           policy.Summary `json:"summary"`
		DraftRevision     int            `json:"draftRevision"`
		PublishedRevision int            `json:"publishedRevision"`
	}
	json.NewDecoder(pr.Body).Decode(&prev)
	pr.Body.Close()
	ok := postJSON(t, http.MethodPost, ts.URL+"/api/publish", map[string]any{
		"draftRevision":     prev.DraftRevision,
		"publishedRevision": prev.PublishedRevision,
		"summary":           prev.Summary,
	})
	if ok.StatusCode != 200 {
		t.Fatalf("publish status %d", ok.StatusCode)
	}
	ok.Body.Close()

	// Old exception is gone even though its TTL has not elapsed.
	d := mustDecisions(t, ts.URL, "published")
	i, _ := findDecision(d, "base", "doc", "read")
	if d.Rows[i].Evidence.Decision != "deny" || d.Rows[i].Exception != nil {
		t.Fatalf("post-publish row = %+v", d.Rows[i])
	}

	// Late create stamped p1 -> rejected, cannot attach to p2.
	late := createException(t, ts.URL, map[string]any{
		"tuple":             map[string]string{"role": "base", "resource": "doc", "action": "read"},
		"reason":            "late",
		"ttlMinutes":        5,
		"publishedRevision": 1,
	})
	if late.StatusCode != 409 {
		t.Fatalf("late create status = %d, want 409", late.StatusCode)
	}
	var er errResp
	json.NewDecoder(late.Body).Decode(&er)
	late.Body.Close()
	if er.Code != "published_moved" {
		t.Fatalf("late create code = %s", er.Code)
	}

	// Create against p2 works.
	fresh := createException(t, ts.URL, map[string]any{
		"tuple":             map[string]string{"role": "base", "resource": "doc", "action": "read"},
		"reason":            "on new revision",
		"ttlMinutes":        5,
		"publishedRevision": 2,
	})
	if fresh.StatusCode != 201 {
		t.Fatalf("fresh create status = %d", fresh.StatusCode)
	}
	fresh.Body.Close()

	// Reset wipes it too.
	rr := postJSON(t, http.MethodPost, ts.URL+"/api/demo/reset", nil)
	rr.Body.Close()
	lresp, _ := http.Get(ts.URL + "/api/exceptions")
	var list struct {
		Exceptions        []exceptionPayload `json:"exceptions"`
		PublishedRevision int                `json:"publishedRevision"`
	}
	json.NewDecoder(lresp.Body).Decode(&list)
	lresp.Body.Close()
	if list.PublishedRevision != 1 || len(list.Exceptions) != 0 {
		t.Fatalf("after reset = %+v, want empty list at p1", list)
	}
	d = mustDecisions(t, ts.URL, "published")
	i, _ = findDecision(d, "base", "doc", "read")
	if d.Rows[i].Exception != nil || d.Rows[i].Evidence.ExceptionID != "" {
		t.Fatalf("exception survived reset: %+v", d.Rows[i])
	}
}

// TestHTTPConcurrentCreateAndPublish interleaves real HTTP creates with
// publishes and checks the page invariant: every row that reports an
// exception identity carries the exception object and matches the
// current published revision.
func TestHTTPConcurrentCreateAndPublish(t *testing.T) {
	mux := http.NewServeMux()
	srv, _ := clockSrv(t, time.Now())
	srv.Routes(mux)
	ts := newHTTPTS(t, mux)
	defer ts.Close()

	tuple := map[string]string{"role": "base", "resource": "doc", "action": "read"}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Creator hammering p1 (after publish moves to p2 these must all 409).
	wg.Add(1)
	go func() {
		defer wg.Done()
		body := map[string]any{
			"tuple": tuple, "reason": "race",
			"ttlMinutes": 5, "publishedRevision": 1,
		}
		for {
			select {
			case <-stop:
				return
			default:
			}
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						t.Errorf("create panicked: %v", rec)
					}
				}()
				resp, err := http.Post(ts.URL+"/api/exceptions", "application/json",
					jsonReader(body))
				if err == nil {
					resp.Body.Close()
				}
			}()
		}
	}()

	// Reader checking the no-mixed-state invariant continuously.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			resp, err := http.Get(ts.URL + "/api/decisions/published")
			if err != nil {
				continue
			}
			var d decisionsPayload
			decErr := json.NewDecoder(resp.Body).Decode(&d)
			resp.Body.Close()
			if decErr != nil {
				continue
			}
			for _, row := range d.Rows {
				if (row.Evidence.ExceptionID != "") != (row.Exception != nil) {
					t.Errorf("mixed state: id=%q obj=%v", row.Evidence.ExceptionID, row.Exception)
					return
				}
				if row.Exception != nil && row.Exception.PublishedRevision != d.Revision {
					t.Errorf("exception pinned to p%d observed under p%d",
						row.Exception.PublishedRevision, d.Revision)
					return
				}
			}
		}
	}()

	// One publish in the middle (may succeed or lose; either is fine —
	// the invariants are what matter).
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := http.Get(ts.URL + "/api/state")
		if err != nil {
			return
		}
		var st stateResp
		_ = json.NewDecoder(resp.Body).Decode(&st)
		resp.Body.Close()
		doc := st.Draft.Document
		doc.Rules = append(doc.Rules, policy.Rule{
			ID: "concurrent-x", Role: "editor", Resource: "billing", Action: "delete",
			Priority: 40, Effect: policy.EffectAllow,
		})
		sr, err := http.NewRequest(http.MethodPut, ts.URL+"/api/draft", jsonReader(doc))
		if err != nil {
			return
		}
		sr.Header.Set("Content-Type", "application/json")
		if r2, e := http.DefaultClient.Do(sr); e == nil {
			r2.Body.Close()
		}
		pr, err := http.Post(ts.URL+"/api/preview", "application/json", nil)
		if err != nil {
			return
		}
		var prev struct {
			Summary           policy.Summary `json:"summary"`
			DraftRevision     int            `json:"draftRevision"`
			PublishedRevision int            `json:"publishedRevision"`
		}
		_ = json.NewDecoder(pr.Body).Decode(&prev)
		pr.Body.Close()
		pub, err := http.Post(ts.URL+"/api/publish", "application/json", jsonReader(map[string]any{
			"draftRevision":     prev.DraftRevision,
			"publishedRevision": prev.PublishedRevision,
			"summary":           prev.Summary,
		}))
		if err == nil {
			pub.Body.Close()
		}
	}()

	// Let the storm run briefly, then stop.
	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()

	// Final: published at p2 and no p1 exception attached.
	d := mustDecisions(t, ts.URL, "published")
	if d.Revision != 2 {
		t.Fatalf("final revision = %d, want 2", d.Revision)
	}
	i, _ := findDecision(d, "base", "doc", "read")
	if d.Rows[i].Exception != nil || d.Rows[i].Evidence.ExceptionID != "" {
		t.Fatalf("stale p1 exception observable under p2: %+v", d.Rows[i])
	}
}
