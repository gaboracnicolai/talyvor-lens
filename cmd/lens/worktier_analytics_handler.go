package main

// worktier_analytics_handler.go — read-only, admin-gated analytics over the WorkTier
// descriptive classifier (work_tier_observations, 0059). The ANALYTICS/DISPLAY consumer of
// the captured-but-unconsumed tier: GET /v1/admin/worktier/distribution surfaces ONE
// workspace's tier distribution (sliced by model) from worktier.Store.Aggregate.
//
// HARD PROPERTIES (scrutinize):
//   - READ-ONLY. The seam is worktier.Store.Aggregate (Query-only; the store exposes no
//     Exec/Begin to this path and imports no mining/economy) — it cannot mutate anything.
//   - PER-TENANT, single-workspace-scoped. workspace_id is REQUIRED (400 if absent); there is
//     NO all-workspaces mode. Aggregate is WHERE workspace_id=$1 by construction, so A never
//     sees B.
//   - ADMIN-GATED (requireAdmin → 401), NOT economy-gated — registered on `authed`, matching
//     the WorkTier capture's kill-switch-EXEMPT posture and the pool-royalty observability reads.
//   - MONEY-DECOUPLED. WorkTier never feeds mint/earn/billing; this read changes nothing.
//
// The snake_case DTO lives here so internal/worktier stays untouched.

import (
	"context"
	"net/http"
	"time"

	"github.com/talyvor/lens/internal/worktier"
)

const defaultWorkTierWindow = 24 * time.Hour

// worktierAggregator is the read seam — *worktier.Store satisfies it. Aggregate is Query-only
// and per-workspace scoped; the handler has no other reach into the store.
type worktierAggregator interface {
	Aggregate(ctx context.Context, workspaceID string, window time.Duration) ([]worktier.TierCount, error)
}

// workTierCountDTO is one (model, tier) cell of the per-workspace distribution (snake_case wire shape).
type workTierCountDTO struct {
	Model       string `json:"model"`
	SizeBucket  string `json:"size_bucket"`
	CostBucket  string `json:"cost_bucket"`
	Complexity  string `json:"complexity"`
	Sensitivity string `json:"sensitivity"`
	Count       int    `json:"count"`
}

type workTierDistributionResponse struct {
	WorkspaceID   string             `json:"workspace_id"`
	WindowSeconds float64            `json:"window_seconds"`
	Rows          []workTierCountDTO `json:"rows"`
}

// newWorkTierDistributionHandler builds the read-only admin distribution endpoint. requireAdmin
// (the wrapper) is the 401 gate; workspace_id is REQUIRED → 400 (there is no cross-tenant mode).
func newWorkTierDistributionHandler(agg worktierAggregator) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		workspaceID := req.URL.Query().Get("workspace_id")
		if workspaceID == "" {
			writeJSONErr(w, http.StatusBadRequest, "workspace_id is required")
			return
		}
		window := defaultWorkTierWindow
		if raw := req.URL.Query().Get("window"); raw != "" {
			d, err := time.ParseDuration(raw)
			if err != nil || d <= 0 {
				writeJSONErr(w, http.StatusBadRequest, "window must be a positive Go duration (e.g. 24h, 168h)")
				return
			}
			window = d
		}
		counts, err := agg.Aggregate(req.Context(), workspaceID, window)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "worktier distribution failed")
			return
		}
		rows := make([]workTierCountDTO, 0, len(counts))
		for _, c := range counts {
			rows = append(rows, workTierCountDTO{
				Model:       c.Model,
				SizeBucket:  string(c.Size),
				CostBucket:  string(c.Cost),
				Complexity:  string(c.Complexity),
				Sensitivity: string(c.Sensitivity),
				Count:       c.Count,
			})
		}
		writeJSONOK(w, http.StatusOK, workTierDistributionResponse{
			WorkspaceID:   workspaceID,
			WindowSeconds: window.Seconds(),
			Rows:          rows,
		})
	}
}
