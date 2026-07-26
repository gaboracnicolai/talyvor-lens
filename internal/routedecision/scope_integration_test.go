package routedecision

import (
	"context"
	"testing"
	"time"
)

// THE WORKSPACE-FILTER GAP. summarySQL aggregated `WHERE created_at >= $1` and nothing else — every tenant's
// rows in one sum. It is not exposed to customers today, which is exactly why it is worth closing NOW: it is
// the shape someone building the customer-facing savings figure would reach for, and proxying it would hand
// one tenant another tenant's spend. The table has always stored workspace_id and has always carried
// idx_routing_decisions_ws (workspace_id, created_at DESC), so scoping costs one predicate and no migration.
//
// The cross-tenant read still exists for the admin forensic endpoint, but it is now named
// SummarizeAllTenants — you cannot reach it without saying so.

// seedTwoTenants writes 2 rows for wsA (one an override) and 1 for wsB, all cache-aware basis.
func seedTwoTenants(t *testing.T, w *Writer) {
	t.Helper()
	ctx := context.Background()
	for _, r := range []RouteDecision{
		{WorkspaceID: "wsA", BaselineModel: "big", ActualModel: "small", CohortOverrode: true, InputTokens: 100, OutputTokens: 50, ActualCostU: 100, CounterfactualCostEstimateU: 300, CostBasis: BasisCacheAware},
		{WorkspaceID: "wsA", BaselineModel: "small", ActualModel: "small", CohortOverrode: false, InputTokens: 100, OutputTokens: 50, ActualCostU: 100, CounterfactualCostEstimateU: 100, CostBasis: BasisCacheAware},
		{WorkspaceID: "wsB", BaselineModel: "big", ActualModel: "small", CohortOverrode: true, InputTokens: 900, OutputTokens: 900, ActualCostU: 7_000, CounterfactualCostEstimateU: 9_000, CostBasis: BasisCacheAware},
	} {
		if err := w.Record(ctx, r); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
}

// ⭐ RED-FIRST, the assertion the brief asks for: two workspaces, each sees ONLY its own rows.
func TestSummarize_IsWorkspaceScoped_Integration(t *testing.T) {
	pool := routePool(t)
	ctx := context.Background()
	seedTwoTenants(t, NewWriter(pool))
	r := NewReader(pool)
	since := time.Now().Add(-time.Hour)

	a, err := r.Summarize(ctx, "wsA", since)
	if err != nil {
		t.Fatalf("summarize wsA: %v", err)
	}
	if a.TotalRequests != 2 || a.OverrideCount != 1 {
		t.Errorf("wsA total/override = %d/%d, want 2/1", a.TotalRequests, a.OverrideCount)
	}
	// wsA's own delta = (300+100) − (100+100) = 200. wsB's 2_000 must NOT appear.
	if a.EstimatedCostDeltaU != 200 {
		t.Errorf("wsA delta = %d, want 200 — wsB's rows leaked into wsA's sum", a.EstimatedCostDeltaU)
	}

	b, err := r.Summarize(ctx, "wsB", since)
	if err != nil {
		t.Fatalf("summarize wsB: %v", err)
	}
	if b.TotalRequests != 1 {
		t.Errorf("wsB total = %d, want 1", b.TotalRequests)
	}
	if b.EstimatedCostDeltaU != 2_000 {
		t.Errorf("wsB delta = %d, want 2000", b.EstimatedCostDeltaU)
	}
	// Neither tenant's figures may equal the cross-tenant total — that would mean the filter did nothing.
	if a.TotalRequests == 3 || b.TotalRequests == 3 {
		t.Error("a tenant saw all 3 rows: the workspace filter is not applied")
	}
}

// An unknown workspace sees an empty summary, not everyone's.
func TestSummarize_UnknownWorkspaceIsEmpty_Integration(t *testing.T) {
	pool := routePool(t)
	seedTwoTenants(t, NewWriter(pool))
	s, err := NewReader(pool).Summarize(context.Background(), "wsNobody", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if s.TotalRequests != 0 || s.EstimatedCostDeltaU != 0 {
		t.Errorf("unknown workspace saw total=%d delta=%d, want 0/0", s.TotalRequests, s.EstimatedCostDeltaU)
	}
}

// An empty workspace id must NOT be a wildcard that silently returns every tenant.
func TestSummarize_EmptyWorkspaceIsNotAWildcard_Integration(t *testing.T) {
	pool := routePool(t)
	seedTwoTenants(t, NewWriter(pool))
	s, err := NewReader(pool).Summarize(context.Background(), "", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if s.TotalRequests != 0 {
		t.Errorf(`Summarize(ctx, "", …) returned %d rows — an empty id must match nothing, never everything`, s.TotalRequests)
	}
}

// The admin forensic read is preserved, but it has to be asked for by name.
func TestSummarizeAllTenants_StillCrossTenant_Integration(t *testing.T) {
	pool := routePool(t)
	seedTwoTenants(t, NewWriter(pool))
	s, err := NewReader(pool).SummarizeAllTenants(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if s.TotalRequests != 3 || s.OverrideCount != 2 {
		t.Errorf("all-tenants total/override = %d/%d, want 3/2", s.TotalRequests, s.OverrideCount)
	}
	if s.EstimatedCostDeltaU != 2_200 {
		t.Errorf("all-tenants delta = %d, want 2200 (200 + 2000)", s.EstimatedCostDeltaU)
	}
}

// ⭐ THE CHANGEOVER. Rows captured before the cache-aware fix were priced flat, and the cached/uncached split
// they would need to be repriced was NEVER stored — so they are not salvageable. They must therefore not be
// summed together with cache-aware rows: averaging across a basis change is its own false signal. They are
// EXCLUDED from the sums and REPORTED as a count, so the discontinuity is visible instead of silent.
func TestSummarize_ExcludesLegacyFlatRowsAndReportsThem_Integration(t *testing.T) {
	pool := routePool(t)
	ctx := context.Background()
	w := NewWriter(pool)
	seedTwoTenants(t, w)
	// Two legacy rows for wsA, priced the old flat way, with a wildly inflated delta.
	for i := 0; i < 2; i++ {
		if err := w.Record(ctx, RouteDecision{
			WorkspaceID: "wsA", BaselineModel: "big", ActualModel: "small", CohortOverrode: true,
			InputTokens: 100, OutputTokens: 50, ActualCostU: 100, CounterfactualCostEstimateU: 999_999,
			CostBasis: BasisFlat,
		}); err != nil {
			t.Fatal(err)
		}
	}

	s, err := NewReader(pool).Summarize(ctx, "wsA", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if s.TotalRequests != 2 {
		t.Errorf("total = %d, want 2 — legacy flat rows must not be counted in the aggregate", s.TotalRequests)
	}
	if s.EstimatedCostDeltaU != 200 {
		t.Errorf("delta = %d, want 200 — a legacy flat row's inflated estimate leaked into the sum", s.EstimatedCostDeltaU)
	}
	if s.ExcludedLegacyBasisRows != 2 {
		t.Errorf("ExcludedLegacyBasisRows = %d, want 2 — excluded history must be REPORTED, not silently dropped",
			s.ExcludedLegacyBasisRows)
	}
}
