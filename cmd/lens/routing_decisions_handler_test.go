package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/routedecision"
)

// The admin summary endpoint is CROSS-TENANT by design (forensic, requireAdmin-gated). When Summarize became
// workspace-scoped, this handler had to move to the explicitly-named SummarizeAllTenants — and nothing pinned
// that. These tests pin it, because the failure mode is silent: a handler accidentally left on the scoped read
// would report one arbitrary tenant's numbers as if they were the fleet's.

type fakeAllTenantsSummarizer struct {
	calls  int
	since  time.Time
	result routedecision.Summary
}

func (f *fakeAllTenantsSummarizer) SummarizeAllTenants(_ context.Context, since time.Time) (routedecision.Summary, error) {
	f.calls++
	f.since = since
	return f.result, nil
}

func serveSummary(t *testing.T, f *fakeAllTenantsSummarizer, url string) map[string]any {
	t.Helper()
	now := func() time.Time { return time.Unix(1_800_000_000, 0) }
	rec := httptest.NewRecorder()
	newRoutingDecisionsSummaryHandler(f, now)(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	return body
}

// It reads the CROSS-TENANT summary, and says so in the payload so no one proxies it to a customer.
func TestRoutingDecisionsSummary_UsesCrossTenantRead(t *testing.T) {
	f := &fakeAllTenantsSummarizer{result: routedecision.Summary{
		TotalRequests: 10, OverrideCount: 4, OverrideRate: 0.4,
		TotalActualCostU: 1_000, TotalCounterfactualEstimateU: 1_600, EstimatedCostDeltaU: 600,
	}}
	body := serveSummary(t, f, "/v1/admin/routing-decisions/summary")

	if f.calls != 1 {
		t.Fatalf("SummarizeAllTenants called %d times, want 1", f.calls)
	}
	if body["total_requests"] != float64(10) || body["estimated_cost_delta_u"] != float64(600) {
		t.Errorf("aggregate not passed through: %v", body)
	}
	scope, _ := body["scope"].(string)
	if scope == "" {
		t.Error(`no "scope" field — an all-tenants payload must announce that it is all-tenants`)
	}
}

// ⭐ The excluded-legacy count must reach the response. If it is dropped, a reader sees an aggregate with no
// hint that flat-basis history exists beside it — which is the silent basis change the label exists to prevent.
func TestRoutingDecisionsSummary_ReportsExcludedLegacyRows(t *testing.T) {
	f := &fakeAllTenantsSummarizer{result: routedecision.Summary{TotalRequests: 3, ExcludedLegacyBasisRows: 41}}
	body := serveSummary(t, f, "/v1/admin/routing-decisions/summary")

	if body["excluded_legacy_basis_rows"] != float64(41) {
		t.Errorf("excluded_legacy_basis_rows = %v, want 41 — excluded history must be visible in the readout", body["excluded_legacy_basis_rows"])
	}
}

// The note has to carry the limits, since this payload is the thing someone would quote from.
func TestRoutingDecisionsSummary_NoteStatesTheDenominatorLimits(t *testing.T) {
	f := &fakeAllTenantsSummarizer{result: routedecision.Summary{TotalRequests: 1}}
	note, _ := serveSummary(t, f, "/v1/admin/routing-decisions/summary")["note"].(string)

	for _, want := range []string{"ESTIMATE", "NOT ALL TRAFFIC", "auto-routed", "shed under load"} {
		if !strings.Contains(note, want) {
			t.Errorf("note is missing %q — a reader could quote a percentage without knowing its denominator.\nnote: %s", want, note)
		}
	}
}

// The window parameter still parses, and an invalid one falls back rather than erroring.
func TestRoutingDecisionsSummary_WindowParsing(t *testing.T) {
	f := &fakeAllTenantsSummarizer{}
	now := time.Unix(1_800_000_000, 0)

	body := serveSummary(t, f, "/v1/admin/routing-decisions/summary?window=2h")
	if body["window"] != "2h0m0s" {
		t.Errorf("window = %v, want 2h0m0s", body["window"])
	}
	if !f.since.Equal(now.Add(-2 * time.Hour)) {
		t.Errorf("since = %v, want %v", f.since, now.Add(-2*time.Hour))
	}

	f2 := &fakeAllTenantsSummarizer{}
	if b := serveSummary(t, f2, "/v1/admin/routing-decisions/summary?window=nonsense"); b["window"] != "24h0m0s" {
		t.Errorf("garbage window = %v, want the 24h default", b["window"])
	}
}
