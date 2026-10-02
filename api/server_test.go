package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"accesssim/policy"
)

func newTestServer(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	mux := http.NewServeMux()
	srv := NewDefaultServer()
	srv.Routes(mux)
	ts := httptest.NewServer(mux)
	return ts, ts.Close
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

func postJSON(t *testing.T, method, url string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, url, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestStateAndSimulationNotice(t *testing.T) {
	ts, done := newTestServer(t)
	defer done()

	resp, err := http.Get(ts.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st stateResp
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.Draft.Revision != 1 || st.Published.Revision != 1 {
		t.Fatalf("initial revisions = %d/%d, want 1/1", st.Draft.Revision, st.Published.Revision)
	}
	if len(st.Draft.Document.Roles) != 4 {
		t.Fatalf("demo roles = %d, want 4 (diamond)", len(st.Draft.Document.Roles))
	}
	if st.Notice == "" {
		t.Fatal("missing simulation-only notice")
	}
}

// TestHTTPEndToEndPreviewPublish exercises save -> preview -> publish,
// including rejection of a stale preview after another edit.
func TestHTTPEndToEndPreviewPublish(t *testing.T) {
	ts, done := newTestServer(t)
	defer done()

	// Fetch current draft, tweak a rule (raise viewer rule to billing
	// read via wildcard-free explicit rule), save.
	resp, _ := http.Get(ts.URL + "/api/state")
	var st stateResp
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()

	doc := st.Draft.Document
	doc.Rules = append(doc.Rules, policy.Rule{
		ID: "test-new", Role: "viewer", Resource: "auditlog", Action: "read",
		Priority: 50, Effect: policy.EffectAllow,
	})

	saveResp := postJSON(t, http.MethodPut, ts.URL+"/api/draft", doc)
	var saved struct {
		Draft struct {
			Revision int `json:"revision"`
		} `json:"draft"`
	}
	json.NewDecoder(saveResp.Body).Decode(&saved)
	saveResp.Body.Close()
	if saved.Draft.Revision != 2 {
		t.Fatalf("draft revision after save = %d, want 2", saved.Draft.Revision)
	}

	// Preview.
	prevResp := postJSON(t, http.MethodPost, ts.URL+"/api/preview", nil)
	var preview struct {
		Summary           policy.Summary `json:"summary"`
		DraftRevision     int            `json:"draftRevision"`
		PublishedRevision int            `json:"publishedRevision"`
	}
	json.NewDecoder(prevResp.Body).Decode(&preview)
	prevResp.Body.Close()
	if len(preview.Summary.NewAllows) == 0 {
		t.Fatal("expected at least one new allow from test-new rule")
	}

	// Editing the draft again must invalidate the preview.
	doc2 := doc
	doc2.Rules = append(doc2.Rules, policy.Rule{
		ID: "test-late", Role: "admin", Resource: "billing", Action: "write",
		Priority: 60, Effect: policy.EffectDeny,
	})
	r2 := postJSON(t, http.MethodPut, ts.URL+"/api/draft", doc2)
	r2.Body.Close()

	bad := postJSON(t, http.MethodPost, ts.URL+"/api/publish", map[string]any{
		"draftRevision":     preview.DraftRevision,
		"publishedRevision": preview.PublishedRevision,
		"summary":           preview.Summary,
	})
	if bad.StatusCode != http.StatusConflict {
		t.Fatalf("stale publish status = %d, want 409", bad.StatusCode)
	}
	var eb errResp
	json.NewDecoder(bad.Body).Decode(&eb)
	bad.Body.Close()
	if eb.Code != "stale_draft_preview" {
		t.Fatalf("error code = %s, want stale_draft_preview", eb.Code)
	}

	// Fresh preview -> publish succeeds.
	freshResp := postJSON(t, http.MethodPost, ts.URL+"/api/preview", nil)
	var fresh struct {
		Summary           policy.Summary `json:"summary"`
		DraftRevision     int            `json:"draftRevision"`
		PublishedRevision int            `json:"publishedRevision"`
	}
	json.NewDecoder(freshResp.Body).Decode(&fresh)
	freshResp.Body.Close()

	okResp := postJSON(t, http.MethodPost, ts.URL+"/api/publish", map[string]any{
		"draftRevision":     fresh.DraftRevision,
		"publishedRevision": fresh.PublishedRevision,
		"summary":           fresh.Summary,
	})
	if okResp.StatusCode != http.StatusOK {
		buf := make([]byte, 4096)
		n, _ := okResp.Body.Read(buf)
		t.Fatalf("publish status = %d: %s", okResp.StatusCode, buf[:n])
	}
	okResp.Body.Close()
}

// TestHTTPConcurrentPublishers races two real HTTP clients on the same
// revision pair: exactly one gets 200.
func TestHTTPConcurrentPublishers(t *testing.T) {
	ts, done := newTestServer(t)
	defer done()

	// Get the draft, add a rule, save.
	resp, _ := http.Get(ts.URL + "/api/state")
	var st stateResp
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()

	doc := st.Draft.Document
	doc.Rules = append(doc.Rules, policy.Rule{
		ID: "race-rule", Role: "editor", Resource: "auditlog", Action: "write",
		Priority: 40, Effect: policy.EffectAllow,
	})
	sr := postJSON(t, http.MethodPut, ts.URL+"/api/draft", doc)
	sr.Body.Close()

	// Both clients preview the same state.
	previewBody := func() (policy.Summary, int, int) {
		r := postJSON(t, http.MethodPost, ts.URL+"/api/preview", nil)
		var p struct {
			Summary           policy.Summary `json:"summary"`
			DraftRevision     int            `json:"draftRevision"`
			PublishedRevision int            `json:"publishedRevision"`
		}
		json.NewDecoder(r.Body).Decode(&p)
		r.Body.Close()
		return p.Summary, p.DraftRevision, p.PublishedRevision
	}
	sumA, drA, prA := previewBody()
	sumB, drB, prB := previewBody()

	publish := func(sum policy.Summary, dr, pr int) int {
		r := postJSON(t, http.MethodPost, ts.URL+"/api/publish", map[string]any{
			"draftRevision":     dr,
			"publishedRevision": pr,
			"summary":           sum,
		})
		code := r.StatusCode
		r.Body.Close()
		return code
	}

	var wg sync.WaitGroup
	codes := make(chan int, 2)
	wg.Add(2)
	go func() { defer wg.Done(); codes <- publish(sumA, drA, prA) }()
	go func() { defer wg.Done(); codes <- publish(sumB, drB, prB) }()
	wg.Wait()
	close(codes)

	var ok, conflict int
	for c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		default:
			t.Fatalf("unexpected publish status %d", c)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("results: ok=%d conflict=%d, want exactly one of each", ok, conflict)
	}
}

func TestHTTPInvalidDocument(t *testing.T) {
	ts, done := newTestServer(t)
	defer done()
	bad := DemoDocument()
	bad.Roles = append(bad.Roles,
		policy.Role{Name: "ninth"},
		policy.Role{Name: "tenth"},
		policy.Role{Name: "eleventh"},
		policy.Role{Name: "twelfth"},
		policy.Role{Name: "thirteenth"},
	)
	resp := postJSON(t, http.MethodPut, ts.URL+"/api/draft", bad)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestHTTPSummaryHashTampering(t *testing.T) {
	ts, done := newTestServer(t)
	defer done()

	r := postJSON(t, http.MethodPost, ts.URL+"/api/preview", nil)
	var p struct {
		Summary           policy.Summary `json:"summary"`
		DraftRevision     int            `json:"draftRevision"`
		PublishedRevision int            `json:"publishedRevision"`
	}
	json.NewDecoder(r.Body).Decode(&p)
	r.Body.Close()

	// Tamper with evidence but keep revision numbers.
	p.Summary.NewAllows = append(p.Summary.NewAllows, policy.DiffEntry{
		Tuple:  policy.Tuple{Role: "forged", Resource: "doc", Action: "read"},
		Before: policy.Evidence{Decision: "deny"},
		After:  policy.Evidence{Decision: "allow"},
		FromTo: "deny->allow",
	})
	resp := postJSON(t, http.MethodPost, ts.URL+"/api/publish", map[string]any{
		"draftRevision":     p.DraftRevision,
		"publishedRevision": p.PublishedRevision,
		"summary":           p.Summary,
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("tampered publish status = %d, want 422", resp.StatusCode)
	}
	resp.Body.Close()
}
