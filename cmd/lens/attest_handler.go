package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/attest"
)

// attestor is the reproduce-before-burn seam (*attest.Attestor satisfies it).
type attestor interface {
	Attest(ctx context.Context, outputID string, treeTar []byte) (attest.Result, error)
}

// newAttestHandler serves POST /v1/admin/attest/{output_id} — ADMIN-ONLY (wrapped in requireAdmin at wiring,
// and NEVER placed in the workspace-authed group). A workspace can never trigger, influence, or forge a
// talyvor_verified write: it cannot reach this route, the source is bound by hash to the output's committed
// response, and the write hard-codes verdict_source + attributes the row to the output's OWNER. The request
// body is the candidate source tree (tar). Registered only when LENS_H5_ATTEST_ENABLED.
func newAttestHandler(a attestor) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		outputID := chi.URLParam(req, "output_id")
		if outputID == "" {
			writeJSONErr(w, http.StatusBadRequest, "missing output_id")
			return
		}
		tree, err := io.ReadAll(io.LimitReader(req.Body, 128<<20))
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, "could not read source archive")
			return
		}
		res, err := a.Attest(req.Context(), outputID, tree)
		if err != nil {
			slog.Warn("attest failed", slog.String("output_id", outputID), slog.String("err", err.Error()))
			writeJSONErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{
			"outcome":  res.Outcome,
			"verdict":  res.Verdict,
			"recorded": res.Recorded,
			"reason":   res.Reason,
		})
	}
}
