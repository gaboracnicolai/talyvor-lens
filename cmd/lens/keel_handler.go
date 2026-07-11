package main

import (
	"context"
	"net/http"

	"github.com/talyvor/lens/internal/keel"
)

// keelFindingsLister is the read seam for the admin drift-findings endpoint (Query-only; *keel.Reader
// satisfies it). Kept an interface so the requireAdmin gate is testable without a DB.
type keelFindingsLister interface {
	ListFindings(ctx context.Context, limit int) ([]keel.ListedFinding, error)
}

// newKeelFindingsHandler serves the recorded drift findings as JSON. Wrapped by requireAdmin at the mount
// site — a tenant must never read another tenant's drift attribution. Rows carry only a self workspace +
// cohort aggregates (no counterparty raw value), so an admin forensic read leaks nothing cross-tenant.
func newKeelFindingsHandler(l keelFindingsLister) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		findings, err := l.ListFindings(req.Context(), 100)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if findings == nil {
			findings = []keel.ListedFinding{}
		}
		writeJSONOK(w, http.StatusOK, findings)
	}
}
