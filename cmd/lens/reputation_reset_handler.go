package main

// reputation_reset_handler.go — the admin re-entry endpoint for annotation reputation
// (PR2). POST /v1/admin/annotation-reputation/reset un-benches an annotator whose reputation
// fell below the access floor: it APPENDS an admin_reset event that lands them back at the
// baseline (NOT an UPDATE/DELETE — the 0066 append-only trigger stays intact). Admin-gated by
// requireAdmin (→401), the SAME lock as the observability endpoints. MONEY-DECOUPLED: it moves
// a reputation score, never the ledger.

import (
	"context"
	"encoding/json"
	"net/http"
)

// reputationResetter is the write surface — *mining.ReputationStore.Reset satisfies it. The
// handler can ONLY reset; it has no ledger/credit reach.
type reputationResetter interface {
	Reset(ctx context.Context, annotatorID, by, note string) (float64, error)
}

type reputationResetRequest struct {
	AnnotatorID string `json:"annotator_id"`
	Reason      string `json:"reason"`
}

// newReputationResetHandler builds the admin reset endpoint. requireAdmin (the wrapper) is the
// 401 gate; this re-reads the AuthContext only to stamp the audit `by` on the appended event.
func newReputationResetHandler(am adjudicateAuthenticator, store reputationResetter) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		by := "global_key"
		if actx, err := am.Authenticate(req); err == nil && actx != nil && actx.UserID != "" {
			by = actx.UserID
		}
		var body reputationResetRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeJSONErr(rw, http.StatusBadRequest, "invalid request body")
			return
		}
		if body.AnnotatorID == "" {
			writeJSONErr(rw, http.StatusBadRequest, "annotator_id required")
			return
		}
		score, err := store.Reset(req.Context(), body.AnnotatorID, by, body.Reason)
		if err != nil {
			writeJSONErr(rw, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(rw, http.StatusOK, map[string]any{"annotator_id": body.AnnotatorID, "reputation": score})
	}
}
