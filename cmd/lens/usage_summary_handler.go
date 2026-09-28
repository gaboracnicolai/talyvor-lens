package main

import (
	"context"
	"net/http"
	"time"

	"github.com/talyvor/lens/internal/opsusage"
	"github.com/talyvor/lens/internal/workspace"
)

// usageSummarizer is the read seam for GET /v1/admin/usage/summary (*opsusage.Reader satisfies it).
type usageSummarizer interface {
	Summarize(ctx context.Context, audience workspace.Audience, since time.Time) (opsusage.Summary, error)
}

// newAdminUsageSummaryHandler serves GET /v1/admin/usage/summary — the operator's usage and spend totals
// across every workspace over a window (default 24h, ?window=<Go duration>): requests, tokens, provider
// cost, serves by source (model, own cache, shared pool) and workspace counts. Synthetic workspaces are
// left out; ?synthetic=only reads them alone, which is how the test harness sees its own traffic (B17.7).
// Cross-tenant, so requireAdminOrOperatorRead-gated at the mount site.
func newAdminUsageSummaryHandler(r usageSummarizer, now func() time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		audience, err := workspace.ParseAudience(req.URL.Query().Get(workspace.AudienceQueryParam))
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		window := 24 * time.Hour
		if q := req.URL.Query().Get("window"); q != "" {
			d, err := time.ParseDuration(q)
			if err != nil || d <= 0 {
				writeJSONErr(w, http.StatusBadRequest, "window must be a positive duration, e.g. 720h")
				return
			}
			window = d
		}
		s, err := r.Summarize(req.Context(), audience, now().Add(-window))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{
			"window":  window.String(),
			"summary": s,
			"scope":   "ALL WORKSPACES in the audience — operator read; never serve this to a customer",
		})
	}
}
