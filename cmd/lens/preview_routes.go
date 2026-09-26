package main

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/distillpreview"
	"github.com/talyvor/lens/internal/tare"
)

// tarePreviewMaxBytes caps a Tare preview's body. A preview is a paste, not a document upload.
const tarePreviewMaxBytes = 1 << 20

// mountPreviewRoutes registers the Try-it previews (B11.4): each runs one feature on the caller's input
// WITHOUT a model call and WITHOUT a charge — neither handler holds a ledger, a token_events writer or a
// provider. Mounted inside the authed group, so the credential, the rate limit and
// workspaceIsolationMiddleware's {wsID} binding all apply.
func mountPreviewRoutes(r chi.Router, conv distillpreview.Converter) {
	r.Post("/v1/workspaces/{wsID}/tare/preview", tarePreviewHandler)

	// The admin preview's handler and converter, reached by the workspace's own credential:
	// workspaceIsolationMiddleware has already refused any caller not bound to {wsID}.
	distill := &distillpreview.Handler{Converter: conv, IsAdmin: func(*http.Request) bool { return true }}
	r.Post("/v1/workspaces/{wsID}/distill/preview", distill.ServeHTTP)
}

// tarePreviewHandler answers POST /v1/workspaces/{wsID}/tare/preview {content, kind, model?} with what
// Tare would send upstream in place of content. Every token figure is an estimate and is named so.
func tarePreviewHandler(w http.ResponseWriter, req *http.Request) {
	var in struct {
		Content string `json:"content"`
		Kind    string `json:"kind"`
		Model   string `json:"model"`
	}
	if err := json.NewDecoder(io.LimitReader(req.Body, tarePreviewMaxBytes+1)).Decode(&in); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(in.Content) > tarePreviewMaxBytes {
		writeJSONErr(w, http.StatusRequestEntityTooLarge, "content exceeds 1 MiB")
		return
	}
	kind := tare.Kind(in.Kind)
	switch kind {
	case "", tare.KindJSON, tare.KindCode, tare.KindLog, tare.KindProse, tare.KindUnknown:
	default:
		writeJSONErr(w, http.StatusBadRequest, "kind must be one of json, code, log, prose, unknown, or empty")
		return
	}
	res, err := tare.Preview(req.Context(), []byte(in.Content), kind)
	if err != nil {
		writeJSONErr(w, http.StatusUnprocessableEntity, "tare could not run: "+err.Error())
		return
	}
	out := map[string]any{
		"reduced":                string(res.Reduced),
		"kind":                   string(res.Kind),
		"refused":                res.Refused,
		"refusal_reasons":        res.Reasons,
		"tokens_in_estimated":    res.TokensIn,
		"tokens_out_estimated":   res.TokensOut,
		"tokens_saved_estimated": res.TokensIn - res.TokensOut,
	}
	if in.Model != "" {
		// Priced like tare_delta_cost_usd on a real request: the removed tokens at the model's input rate.
		// A model the catalog does not price gets no figure rather than a saving of $0.
		out["model"] = in.Model
		if inRate, _, ok := catalog.Price(in.Model); ok {
			out["saving_usd_estimated"] = float64(res.TokensIn-res.TokensOut) * inRate / 1_000_000
		}
	}
	writeJSONOK(w, http.StatusOK, out)
}
