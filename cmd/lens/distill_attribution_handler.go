package main

import (
	"context"
	"net/http"
	"strconv"

	"github.com/talyvor/lens/internal/distillattrib"
)

// distill_attribution_handler.go — the ADMIN-ONLY read surface over
// distill_serve_attribution (S1 read-surface commitment). Wired under
// requireAdmin in run(): content_hash + counterparty workspace ids are exposed
// here, so this route must never be tenant-reachable. The tenant-facing MASKED
// view (counts only, no wsID/hash) stays deferred (YAGNI; spec recorded).

const (
	// distillAttribLimitDefault / Max mirror the repo's limit-cap convention
	// (Atoi + n>0 && n<=cap). Higher than tenant caps because this is an admin
	// ops/probe surface; capped so ?limit= can never dump the table unbounded.
	distillAttribLimitDefault = 100
	distillAttribLimitMax     = 1000
)

// distillAttribReader is the slice of *distillattrib.Reader the handler needs.
type distillAttribReader interface {
	RawRows(ctx context.Context, limit int) ([]distillattrib.ServeRow, error)
	PairTotals(ctx context.Context, limit int) ([]distillattrib.PairTotal, error)
}

// newDistillAttributionAdminHandler — GET /v1/admin/distill/attribution. Default
// returns raw rows (most-recent first); ?view=pairs returns the condition-(b)
// materiality aggregate (serve_count per owner/requester pair). ?limit= is
// capped at distillAttribLimitMax.
func newDistillAttributionAdminHandler(reader distillAttribReader) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		limit := distillAttribLimitDefault
		if v := req.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= distillAttribLimitMax {
				limit = n
			}
		}
		if req.URL.Query().Get("view") == "pairs" {
			pairs, err := reader.PairTotals(req.Context(), limit)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, pairs)
			return
		}
		rows, err := reader.RawRows(req.Context(), limit)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, rows)
	}
}
