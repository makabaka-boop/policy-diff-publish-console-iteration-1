package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"accesssim/policy"
	"accesssim/store"
)

// Server wires the store to JSON HTTP handlers. The active store is an
// atomic pointer so the demo reset can swap it without racing requests.
type Server struct {
	store atomic.Pointer[store.Store]
	// clock is the time source inherited by every (re)created store. It
	// exists for deterministic tests; production uses time.Now.
	clock func() time.Time
}

func NewServer(st *store.Store) *Server {
	s := &Server{clock: time.Now}
	s.store.Store(st)
	return s
}

// NewServerWithClock is NewServer with an injected clock. The clock is
// also used by the store created on demo reset.
func NewServerWithClock(st *store.Store, now func() time.Time) *Server {
	if now == nil {
		now = time.Now
	}
	s := &Server{clock: now}
	s.store.Store(st)
	return s
}

func (s *Server) getStore() *store.Store { return s.store.Load() }

// NewDefaultServer seeds the simulator with the diamond-inheritance demo.
func NewDefaultServer() *Server {
	st, err := store.New(DemoDocument())
	if err != nil {
		panic(err) // demo document is compile-time valid
	}
	return NewServer(st)
}

// Routes registers every API endpoint.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("PUT /api/draft", s.handleSaveDraft)
	mux.HandleFunc("POST /api/preview", s.handlePreview)
	mux.HandleFunc("POST /api/publish", s.handlePublish)
	mux.HandleFunc("GET /api/decisions/{version}", s.handleDecisions)
	mux.HandleFunc("GET /api/exceptions", s.handleListExceptions)
	mux.HandleFunc("POST /api/exceptions", s.handleCreateException)
	mux.HandleFunc("POST /api/demo/reset", s.handleReset)
}

type stateResp struct {
	Draft     store.Versioned `json:"draft"`
	Published store.Versioned `json:"published"`
	Notice    string          `json:"notice"`
}

const simulationNotice = "SIMULATION ONLY: this finite-domain policy lab is not an authorization entry point of any real system."

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	draft, published := s.getStore().Snapshot()
	writeJSON(w, http.StatusOK, stateResp{
		Draft: draft, Published: published, Notice: simulationNotice,
	})
}

func (s *Server) handleSaveDraft(w http.ResponseWriter, r *http.Request) {
	var doc policy.Document
	if err := decodeJSON(r, &doc); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	v, err := s.getStore().SaveDraft(&doc)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"draft":   v,
		"message": "draft saved; any earlier preview is now stale and must be regenerated",
	})
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	sum, draftRev, pubRev, err := s.getStore().Preview()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"summary":           sum,
		"draftRevision":     draftRev,
		"publishedRevision": pubRev,
	})
}

type publishReq struct {
	DraftRevision     int             `json:"draftRevision"`
	PublishedRevision int             `json:"publishedRevision"`
	Summary           *policy.Summary `json:"summary"`
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	var req publishReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.Summary == nil {
		writeError(w, http.StatusBadRequest, "publish requires the preview summary object")
		return
	}
	res, err := s.getStore().Publish(req.DraftRevision, req.PublishedRevision, req.Summary)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"published": res.Published,
		"summary":   res.Summary,
		"message":   "published: the presented preview matched draft and published revisions exactly",
	})
}

func (s *Server) handleDecisions(w http.ResponseWriter, r *http.Request) {
	which := r.PathValue("version")
	if which != "draft" && which != "published" {
		writeError(w, http.StatusBadRequest, "version must be draft or published")
		return
	}
	rev, rows, err := s.getStore().Decisions(which)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":  which,
		"revision": rev,
		"rows":     rows,
	})
}

// handleReset restores the demo document (draft = published, revision 1).
// Intended for the UI demo and tests; it atomically swaps the store for a
// freshly seeded one, which discards every old exception immediately — a
// lingering exception can never outlive the reset onto new data.
func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	st, err := store.NewWithClock(DemoDocument(), s.clock)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.store.Store(st)
	draft, published := st.Snapshot()
	writeJSON(w, http.StatusOK, stateResp{Draft: draft, Published: published, Notice: simulationNotice})
}

// handleCreateException creates a simulated emergency exception for one
// exact, currently-denied tuple of the PUBLISHED policy. All validation
// and re-adjudication happen in one locked store transaction; the
// response carries both the exception identity and the effective
// (temporarily allowed) evidence with its preserved base evidence.
func (s *Server) handleCreateException(w http.ResponseWriter, r *http.Request) {
	var req store.ExceptionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	view, evidence, err := s.getStore().CreateException(req)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"exception": view,
		"evidence":  evidence,
		"message":   "simulated emergency exception created: published decision is temporarily allow; the original rule evidence is preserved for teaching",
	})
}

// handleListExceptions lists active emergency exceptions pinned to the
// current published revision (expired ones are pruned on read).
func (s *Server) handleListExceptions(w http.ResponseWriter, r *http.Request) {
	rev, list, err := s.getStore().ListExceptions()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"publishedRevision": rev,
		"exceptions":        list,
	})
}

// ---- helpers ----

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type errResp struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errResp{Error: msg, Code: http.StatusText(status)})
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrInvalidDocument):
		writeJSON(w, http.StatusUnprocessableEntity, errResp{Error: err.Error(), Code: "invalid_document"})
	case errors.Is(err, store.ErrDraftConflict):
		writeJSON(w, http.StatusConflict, errResp{Error: err.Error(), Code: "stale_draft_preview"})
	case errors.Is(err, store.ErrPublishedConflict):
		writeJSON(w, http.StatusConflict, errResp{Error: err.Error(), Code: "published_moved"})
	case errors.Is(err, store.ErrSummaryHashInvalid):
		writeJSON(w, http.StatusUnprocessableEntity, errResp{Error: err.Error(), Code: "tampered_summary"})
	case errors.Is(err, store.ErrSummaryMismatch):
		writeJSON(w, http.StatusConflict, errResp{Error: err.Error(), Code: "summary_mismatch"})
	case errors.Is(err, store.ErrInvalidSummary):
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error(), Code: "invalid_summary"})
	case errors.Is(err, store.ErrInvalidExceptionRequest):
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error(), Code: "invalid_exception_request"})
	case errors.Is(err, store.ErrTupleOutsideDomain):
		writeJSON(w, http.StatusUnprocessableEntity, errResp{Error: err.Error(), Code: "tuple_outside_domain"})
	case errors.Is(err, store.ErrTupleNotDenied):
		writeJSON(w, http.StatusUnprocessableEntity, errResp{Error: err.Error(), Code: "tuple_not_denied"})
	case errors.Is(err, store.ErrExceptionAlreadyActive):
		writeJSON(w, http.StatusConflict, errResp{Error: err.Error(), Code: "exception_already_active"})
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}
