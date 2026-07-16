package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/talyvor/lens/internal/auth"
)

// auth_token_mint_handler.go — the /v1/auth/token mint capability, extracted from an inline
// closure in run() so its admin-scoping invariant is provable at the HTTP layer
// (auth_token_mint_handler_test.go). Behavior is unchanged.
//
// This is the ADMIN-SCOPED minting path (property 1 of the per-workspace-credential fix): only
// the global admin key may mint a JWT bound to an ARBITRARY tenant workspace. Consumers that
// today share the one global key — collapsing every tenant into the empty-workspace bucket
// lens:rl::global:* and the "default" spend bucket — can instead be issued a per-tenant JWT
// here, which AuthMiddleware derives a TRUSTED workspace from (manager.go JWT branch). A
// workspace key/JWT is NOT admin, so it can only mint for itself — enforced by the IsAdmin gate.
func newAuthTokenMintHandler(authManager *auth.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if authManager.PrivateKey() == nil {
			writeJSONErr(w, http.StatusServiceUnavailable, "JWT signing not available")
			return
		}
		// Require global-admin auth via the unified manager. err != nil short-circuits before
		// actx is dereferenced, so a missing/invalid credential is a clean 403 (fail-closed).
		actx, err := authManager.Authenticate(req)
		if err != nil || !actx.IsAdmin {
			writeJSONErr(w, http.StatusForbidden, "admin credentials required")
			return
		}
		var in struct {
			WorkspaceID string   `json:"workspace_id"`
			UserID      string   `json:"user_id"`
			Scopes      []string `json:"scopes"`
			TTLHours    int      `json:"ttl_hours"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if in.WorkspaceID == "" {
			writeJSONErr(w, http.StatusBadRequest, "workspace_id required")
			return
		}
		ttl := auth.ClampTTL(time.Duration(in.TTLHours) * time.Hour)
		tok, err := auth.GenerateToken(in.WorkspaceID, in.UserID, in.Scopes, authManager.PrivateKey(), ttl)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusCreated, map[string]any{
			"token":      tok,
			"expires_at": time.Now().Add(ttl).UTC().Format(time.RFC3339),
		})
	}
}
