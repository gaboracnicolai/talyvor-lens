package main

import (
	"context"
	"net/http"
	"strconv"

	"github.com/talyvor/lens/internal/attribution"
)

// authz_attribution_handlers.go — the #151 fix. GET /v1/attribution/branch and
// /top were keyed by repository/branch name with NO workspace scoping (any
// tenant read any repo's spend). They now derive the workspace from the
// credential (effectiveWorkspaceID: non-admin forced to own, admin honors
// ?workspace_id=) and read the already-workspace-scoped request_attribution
// table via the Store — so a tenant sees only its OWN slice of a shared repo
// name. Extracted to named constructors so the scoping is provable over HTTP.

// branchSpendReader is the slice of *attribution.Store these routes need.
type branchSpendReader interface {
	GetBranchSpendForWorkspace(ctx context.Context, workspaceID, branch, repository string) (*attribution.BranchSpend, error)
	GetTopBranchesForWorkspace(ctx context.Context, workspaceID, repository string, limit int) ([]attribution.BranchSpend, error)
}

func newAttributionBranchHandler(store branchSpendReader) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		branch := req.URL.Query().Get("branch")
		repository := req.URL.Query().Get("repository")
		if branch == "" || repository == "" {
			writeJSONErr(w, http.StatusBadRequest, "branch and repository query params required")
			return
		}
		wsID, _, ok := effectiveWorkspaceID(req, req.URL.Query().Get("workspace_id"))
		if !ok || wsID == "" {
			writeJSONErr(w, http.StatusForbidden, "forbidden: workspace identity required (admin: pass ?workspace_id=)")
			return
		}
		got, err := store.GetBranchSpendForWorkspace(req.Context(), wsID, branch, repository)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if got == nil {
			writeJSONErr(w, http.StatusNotFound, "branch not found")
			return
		}
		writeJSONOK(w, http.StatusOK, got)
	}
}

func newAttributionTopHandler(store branchSpendReader) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		repository := req.URL.Query().Get("repository")
		if repository == "" {
			writeJSONErr(w, http.StatusBadRequest, "repository query param required")
			return
		}
		limit := 10
		if l := req.URL.Query().Get("limit"); l != "" {
			if n, err := strconv.Atoi(l); err == nil && n > 0 {
				limit = n
			}
		}
		if limit > 50 {
			limit = 50
		}
		wsID, _, ok := effectiveWorkspaceID(req, req.URL.Query().Get("workspace_id"))
		if !ok || wsID == "" {
			writeJSONErr(w, http.StatusForbidden, "forbidden: workspace identity required (admin: pass ?workspace_id=)")
			return
		}
		top, err := store.GetTopBranchesForWorkspace(req.Context(), wsID, repository, limit)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, top)
	}
}
