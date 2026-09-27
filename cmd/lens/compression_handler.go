package main

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/workspace"
)

// compressionPolicySetter is the slice of *workspace.Manager PUT .../compression needs.
type compressionPolicySetter interface {
	SetCompressionPolicy(ctx context.Context, wsID string, policy workspace.CompressionPolicy) error
}

// retiredRewriterMessage is what a caller asking for the prompt rewriter is told.
const retiredRewriterMessage = "the prompt rewriter is retired: it saved nothing measurable and corrupted prompts " +
	"(unfenced code lost its indentation). Tare reduces context instead — PUT /v1/workspaces/{wsID}/tare"

// newCompressionPolicyHandler serves PUT /v1/workspaces/{wsID}/compression. B18.6: the prompt rewriter
// is retired, so any policy but "disabled" is refused with 410 Gone; "disabled" is stored as before,
// so a client turning it off still succeeds.
func newCompressionPolicyHandler(ws compressionPolicySetter) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		wsID := chi.URLParam(req, "wsID")
		var in struct {
			CompressionPolicy workspace.CompressionPolicy `json:"compression_policy"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if in.CompressionPolicy != workspace.CompressionDisabled {
			writeJSONErr(w, http.StatusGone, retiredRewriterMessage)
			return
		}
		if err := ws.SetCompressionPolicy(req.Context(), wsID, workspace.CompressionDisabled); err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"ok": true, "compression_policy": workspace.CompressionDisabled})
	}
}
