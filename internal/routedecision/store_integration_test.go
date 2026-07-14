package routedecision

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Real-PG round-trip: Record writes rows, Summarize aggregates the override rate + estimated delta over a
// window. Also pins the tenancy shape — a row names only its own workspace (no counterparty column exists to
// write).
func routePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG routedecision test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), `
		DROP TABLE IF EXISTS routing_decisions;
		CREATE TABLE routing_decisions (
			id BIGSERIAL PRIMARY KEY,
			workspace_id TEXT NOT NULL,
			baseline_model TEXT NOT NULL,
			actual_model TEXT NOT NULL,
			cohort_overrode BOOLEAN NOT NULL,
			cohort_basis TEXT NOT NULL DEFAULT '',
			cohort_n INTEGER NOT NULL DEFAULT 0,
			input_tokens INTEGER NOT NULL,
			output_tokens INTEGER NOT NULL,
			actual_cost_u BIGINT NOT NULL,
			counterfactual_cost_estimate_u BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return pool
}

func TestRecordAndSummarize_Integration(t *testing.T) {
	pool := routePool(t)
	ctx := context.Background()
	w := NewWriter(pool)

	// 3 requests: 1 overrode (cheaper: actual 100 vs counterfactual 300), 2 did not.
	rows := []RouteDecision{
		{WorkspaceID: "wsA", BaselineModel: "big", ActualModel: "small", CohortOverrode: true, CohortN: 12, InputTokens: 100, OutputTokens: 50, ActualCostU: 100, CounterfactualCostEstimateU: 300},
		{WorkspaceID: "wsA", BaselineModel: "small", ActualModel: "small", CohortOverrode: false, InputTokens: 100, OutputTokens: 50, ActualCostU: 100, CounterfactualCostEstimateU: 100},
		{WorkspaceID: "wsB", BaselineModel: "small", ActualModel: "small", CohortOverrode: false, InputTokens: 100, OutputTokens: 50, ActualCostU: 100, CounterfactualCostEstimateU: 100},
	}
	for _, r := range rows {
		if err := w.Record(ctx, r); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	s, err := NewReader(pool).Summarize(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if s.TotalRequests != 3 || s.OverrideCount != 1 {
		t.Fatalf("total/override = %d/%d, want 3/1", s.TotalRequests, s.OverrideCount)
	}
	if s.OverrideRate < 0.333 || s.OverrideRate > 0.334 {
		t.Errorf("override rate = %v, want ~0.333", s.OverrideRate)
	}
	// Estimated delta = Σcounterfactual(500) − Σactual(300) = 200. ESTIMATE only.
	if s.EstimatedCostDeltaU != 200 {
		t.Errorf("estimated delta = %d, want 200", s.EstimatedCostDeltaU)
	}
}

// The window filter excludes older rows (created_at >= since).
func TestSummarize_WindowFilter_Integration(t *testing.T) {
	pool := routePool(t)
	ctx := context.Background()
	// an old row (2h ago) must be excluded by a 1h window
	if _, err := pool.Exec(ctx, `INSERT INTO routing_decisions
		(workspace_id, baseline_model, actual_model, cohort_overrode, input_tokens, output_tokens, actual_cost_u, counterfactual_cost_estimate_u, created_at)
		VALUES ('wsOld','big','small',true,10,10,1,2, now() - interval '2 hours')`); err != nil {
		t.Fatal(err)
	}
	s, err := NewReader(pool).Summarize(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if s.TotalRequests != 0 {
		t.Errorf("total = %d, want 0 (the 2h-old row is outside the 1h window)", s.TotalRequests)
	}
}
