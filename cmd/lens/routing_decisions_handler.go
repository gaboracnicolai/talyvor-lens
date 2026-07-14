package main

import (
	"context"
	"net/http"
	"time"

	"github.com/talyvor/lens/internal/routedecision"
)

// routeDecisionSummarizer is the read seam (*routedecision.Reader satisfies it).
type routeDecisionSummarizer interface {
	Summarize(ctx context.Context, since time.Time) (routedecision.Summary, error)
}

// newRoutingDecisionsSummaryHandler serves GET /v1/admin/routing-decisions/summary — THE go/no-go readout:
// over a window (default 24h, ?window=<Go duration>), how many auto-routed requests, how often the cohort
// OVERRODE the baseline (the override RATE), and the aggregate ESTIMATED cost delta. requireAdmin-gated.
// The estimate is NOT money — the response says so plainly.
func newRoutingDecisionsSummaryHandler(r routeDecisionSummarizer, now func() time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		window := 24 * time.Hour
		if q := req.URL.Query().Get("window"); q != "" {
			if d, err := time.ParseDuration(q); err == nil && d > 0 {
				window = d
			}
		}
		s, err := r.Summarize(req.Context(), now().Add(-window))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{
			"window":                          window.String(),
			"total_requests":                  s.TotalRequests,
			"override_count":                  s.OverrideCount,
			"override_rate":                   s.OverrideRate,
			"total_actual_cost_u":             s.TotalActualCostU,
			"total_counterfactual_estimate_u": s.TotalCounterfactualEstimateU,
			"estimated_cost_delta_u":          s.EstimatedCostDeltaU,
			"note": "counterfactual + delta are ESTIMATES (baseline priced at the ACTUAL token counts), NOT money; " +
				"a KE-1 mint would pay strictly less than this, floored at zero (house-favouring).",
		})
	}
}
