package main

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/eval"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/povi"
	"github.com/talyvor/lens/internal/prompts"
	"github.com/talyvor/lens/internal/session"
)

// authz_handlers_phase2.go extracts the #146 Phase-2 SENSITIVE cross-tenant READ
// routes into named constructors so each route's wiring through
// effectiveWorkspaceID is provable over HTTP (the Phase-1 standard). run()
// registers these exact constructors; behavior is unchanged but for the fix.

// applyPhase2WSID resolves the workspace a read may scope to from the
// authenticated credential, not caller input: a NON-ADMIN is forced to its OWN
// workspace (the `requested` value is ignored AND the legacy "default" fallback
// never applies); the global ADMIN honors `requested` (empty → the historical
// "default"). ok is false when a non-admin has no resolvable workspace (403).
func applyPhase2WSID(req *http.Request, requested string) (string, bool) {
	eff, isAdmin, ok := effectiveWorkspaceID(req, requested)
	if !ok {
		return "", false
	}
	if isAdmin && eff == "" {
		eff = "default" // preserve the historical admin-wide default
	}
	return eff, true
}

func phase2Forbidden(w http.ResponseWriter) {
	writeJSONErr(w, http.StatusForbidden, "forbidden: no workspace identity")
}

type sessionLister interface {
	ListActiveByWorkspace(workspaceID string) []*session.Session
}

// newSessionsListHandler — GET /v1/sessions. A non-admin lists only its OWN
// active sessions.
func newSessionsListHandler(lister sessionLister) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		wsID, ok := applyPhase2WSID(req, req.URL.Query().Get("workspace_id"))
		if !ok {
			phase2Forbidden(w)
			return
		}
		writeJSONOK(w, http.StatusOK, lister.ListActiveByWorkspace(wsID))
	}
}

type evalRunsLister interface {
	ListRuns(ctx context.Context, workspaceID string, limit int) ([]eval.RunSummary, error)
}

// newEvalRunsListHandler — GET /v1/eval/runs. A non-admin sees only its OWN runs.
func newEvalRunsListHandler(lister evalRunsLister) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		wsID, ok := applyPhase2WSID(req, req.URL.Query().Get("workspace_id"))
		if !ok {
			phase2Forbidden(w)
			return
		}
		runs, err := lister.ListRuns(req.Context(), wsID, 10)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, runs)
	}
}

type poviReceiptsLister interface {
	ListReceipts(ctx context.Context, workspaceID string, limit int) ([]povi.StoredReceipt, error)
}

// newPoviReceiptsHandler — GET /v1/povi/receipts. NOTE the param is "workspace"
// (not "workspace_id"). A non-admin reads only its OWN inference receipts.
func newPoviReceiptsHandler(lister poviReceiptsLister) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		wsID, ok := applyPhase2WSID(req, req.URL.Query().Get("workspace"))
		if !ok {
			phase2Forbidden(w)
			return
		}
		list, err := lister.ListReceipts(req.Context(), wsID, 50)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, list)
	}
}

// newGuardrailsPolicyGetHandler — GET /v1/guardrails/policy. A non-admin reads
// only its OWN guardrail policy.
func newGuardrailsPolicyGetHandler(engine *guardrails.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		wsID, ok := applyPhase2WSID(req, req.URL.Query().Get("workspace_id"))
		if !ok {
			phase2Forbidden(w)
			return
		}
		writeJSONOK(w, http.StatusOK, engine.GetPolicy(wsID))
	}
}

type promptGetter interface {
	Get(ctx context.Context, name, workspaceID string) (*prompts.Prompt, error)
}

// newPromptGetHandler — GET /v1/prompts/{name}. A non-admin reads only its OWN
// prompt.
func newPromptGetHandler(getter promptGetter) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		name := chi.URLParam(req, "name")
		wsID, ok := applyPhase2WSID(req, req.URL.Query().Get("workspace_id"))
		if !ok {
			phase2Forbidden(w)
			return
		}
		pr, err := getter.Get(req.Context(), name, wsID)
		if err != nil {
			writeJSONErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, pr)
	}
}
