package main

import (
	"context"
	"net/http"

	"github.com/talyvor/lens/internal/workspace"
)

// B18.17 — the operator screen's per-workspace reads: spend, held LENS and last activity, one row per
// workspace in the audience (?audience=synthetic for B17.1's synthetic workspaces). Gated by
// requireAdminOrOperatorRead: the global admin key or the suite's LENS_OPERATOR_READ_KEY, GET only.
func newOperatorWorkspaceReadHandler[T any](read func(context.Context, workspace.Audience) ([]T, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		audience, err := workspace.ParseAudience(req.URL.Query().Get(workspace.AudienceQueryParam))
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		rows, err := read(req.Context(), audience)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{
			"audience":   audience.String(),
			"workspaces": rows,
			"scope":      "ALL WORKSPACES in the audience — operator read; never serve this to a customer",
		})
	}
}
