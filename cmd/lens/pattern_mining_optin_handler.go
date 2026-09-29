package main

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// patternMiningOptInStatus answers GET /v1/workspaces/{wsID}/pattern-mining/opt-in (B18.54): whether the
// workspace shares its routing patterns, read from workspace_pattern_optin, and whether this deployment
// mines patterns at all. When it does not ("enabled": false) the opt-in POST answers 503, so a screen
// shows the state without offering a switch.
func patternMiningOptInStatus(miner interface {
	IsOptedIn(ctx context.Context, workspaceID string) (bool, error)
}, enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		optedIn, err := miner.IsOptedIn(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]bool{"opted_in": optedIn, "enabled": enabled})
	}
}
