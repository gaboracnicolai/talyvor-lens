package main

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/talyvor/lens/internal/workspace"
)

// registrar is the workspace-registration seam (*workspace.Manager satisfies it). Extracted so the
// POST /v1/workspaces handler is testable and can be wrapped in requireAdmin at the route.
type registrar interface {
	RegisterWorkspace(ctx context.Context, ws workspace.Workspace) error
}

// newRegisterWorkspaceHandler serves POST /v1/workspaces — provisioning/registration of a workspace
// by id. Extracted VERBATIM from the inline run() closure so its route can be wrapped in
// requireAdmin (the cross-tenant IDOR fix): RegisterWorkspace is a blind upsert on the body-supplied
// id, so an ungated route let any non-admin overwrite another tenant's config. This handler is
// unchanged; the admin gate is applied at the route (main.go), matching the ~30 sibling
// control-plane routes. No money/ledger path is touched here.
func newRegisterWorkspaceHandler(reg registrar) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var in workspace.Workspace
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if err := reg.RegisterWorkspace(req.Context(), in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSONOK(w, http.StatusCreated, map[string]string{"id": in.ID})
	}
}
