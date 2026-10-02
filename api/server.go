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
}

func NewServer(st *store.Store) *Server {
	s := &Server{}
	s.store.Store(st)
	return s
}

func (s *Server) getStore() *store.Store { return s.store.Load() }

// SwapStore replaces the active store atomically (shared with reset).
func (s *Server) SwapStore(st *store.Store) { s.store.Store(st) }

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

// ---- emergency exceptions ----

type tupleReq struct {
	Role     string `json:"role"`
	Resource string `json:"resource"`
	Action   string `json:"action"`
}

type createExceptionReq struct {
	Tuple             tupleReq `json:"tuple"`
	Reason            string   `json:"reason"`
	TTLMinutes        int      `json:"ttlMinutes"`
	PublishedRevision int      `json:"publishedRevision"`
}

// handleCreateException grants a simulated 1–60 minute emergency
// exception for exactly one published tuple. The store re-adjudicates
// the tuple against the current published revision inside its mutex:
// only a tuple that is currently denied can get one, and a request
// stamped with an outdated publishedRevision is refused.
func (s *Server) handleCreateException(w http.ResponseWriter, r *http.Request) {
	var req createExceptionReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	res, err := s.getStore().CreateEmergencyException(&store.CreateExceptionRequest{
		Tuple: policy.Tuple{
			Role: req.Tuple.Role, Resource: req.Tuple.Resource, Action: req.Tuple.Action,
		},
		Reason:            req.Reason,
		TTLMinutes:        req.TTLMinutes,
		PublishedRevision: req.PublishedRevision,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"exception": res.Exception,
		"decision":  res.Decision,
		"now":       res.Now.Format(time.RFC3339),
		"message":   "SIMULATED emergency exception created: published decision is temporarily allow; the original deny rule evidence remains attached, and this is NOT a published rule change",
	})
}

// handleListExceptions returns active exceptions plus the server-side
// adjudication clock so the page can render countdowns consistently.
func (s *Server) handleListExceptions(w http.ResponseWriter, r *http.Request) {
	pubRev, now, exceptions, err := s.getStore().ListExceptions()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"exceptions":        exceptions,
		"publishedRevision": pubRev,
		"now":               now.Format(time.RFC3339),
	})
}

// handleReset restores the demo document (draft = published, revision 1).
// Intended for the UI demo and tests; it atomically swaps the store,
// which also drops every emergency exception immediately.
func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	st, err := store.New(DemoDocument())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.store.Store(st)
	draft, published := st.Snapshot()
	writeJSON(w, http.StatusOK, stateResp{Draft: draft, Published: published, Notice: simulationNotice})
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
	case errors.Is(err, store.ErrInvalidException):
		writeJSON(w, http.StatusUnprocessableEntity, errResp{Error: err.Error(), Code: "invalid_exception"})
	case errors.Is(err, store.ErrTupleNotDenied):
		writeJSON(w, http.StatusConflict, errResp{Error: err.Error(), Code: "tuple_not_denied"})
	case errors.Is(err, store.ErrExceptionExists):
		writeJSON(w, http.StatusConflict, errResp{Error: err.Error(), Code: "exception_exists"})
	case errors.Is(err, store.ErrExceptionRevisionMoved):
		writeJSON(w, http.StatusConflict, errResp{Error: err.Error(), Code: "published_moved"})
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}
