package main

import (
	"context"
	"net/http"
	"time"

	"github.com/talyvor/lens/internal/haircutobs"
)

// haircutLister is the read seam (*haircutobs.Reader satisfies it).
type haircutLister interface {
	Recent(ctx context.Context, since time.Time) ([]haircutobs.HaircutEvent, error)
}

// newKeelHaircutsHandler serves GET /v1/admin/keel/haircuts — the KE-2 observability surface: over a window
// (default 24h, ?window=<Go duration>), every APPLIED drift haircut (workspace, factor, base→effective µLENS,
// mint type) joined to the hardened finding that caused it. Admin-only. Read-only; moves no money.
func newKeelHaircutsHandler(r haircutLister, now func() time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		window := 24 * time.Hour
		if q := req.URL.Query().Get("window"); q != "" {
			if d, err := time.ParseDuration(q); err == nil && d > 0 {
				window = d
			}
		}
		events, err := r.Recent(req.Context(), now().Add(-window))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{
			"window":   window.String(),
			"count":    len(events),
			"haircuts": events,
			"note": "KE-2 reduce-only drift haircut on bonded reuse royalties; factor is clamped to [0.5, 1.0] " +
				"and can only lower a mint. The threshold is an UNCALIBRATED PLACEHOLDER — recalibrate at N3 before any external contributor earns.",
		})
	}
}
