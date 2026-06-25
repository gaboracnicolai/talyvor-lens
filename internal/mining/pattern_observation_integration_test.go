package mining

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Real-PG: the opt-in WRITE gate + earned=0 proof (the double-gate's opt-in
// half). An opted-in workspace gets exactly one row (opted_in=true, earned=0,
// Advisor-visible); a NON-opted-in workspace gets NO row. And earned is 0 —
// capture never credits.
func TestRecordPatternObservation_OptInWriteGate_Integration(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG pattern-capture test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS routing_patterns`,
		`DROP TABLE IF EXISTS workspace_pattern_optin`,
		`CREATE TABLE routing_patterns (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			workspace_id TEXT NOT NULL, feature_category TEXT NOT NULL, model_used TEXT NOT NULL,
			provider_used TEXT NOT NULL, input_token_range TEXT NOT NULL, output_quality DOUBLE PRECISION NOT NULL DEFAULT 0,
			latency_bucket TEXT NOT NULL, cache_hit_rate DOUBLE PRECISION NOT NULL DEFAULT 0,
			success_rate DOUBLE PRECISION NOT NULL DEFAULT 1, sample_count INT NOT NULL DEFAULT 1,
			rarity DOUBLE PRECISION NOT NULL DEFAULT 0, complexity_bucket TEXT NOT NULL DEFAULT '', opted_in BOOLEAN NOT NULL DEFAULT FALSE,
			earned DOUBLE PRECISION NOT NULL DEFAULT 0, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE workspace_pattern_optin (workspace_id TEXT PRIMARY KEY, opted_in_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`INSERT INTO workspace_pattern_optin (workspace_id) VALUES ('wsA')`, // wsA opted in; wsB not
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}

	m := NewPatternMiner(nil, pool) // NIL ledger — capture cannot mint
	p := RoutingPattern{FeatureCategory: "chat", ModelUsed: "gpt-4o", ProviderUsed: "openai", InputTokenRange: "0-1k", OutputQuality: 0.9, LatencyBucket: "fast", CacheHitRate: 0, SuccessRate: 1, SampleCount: 1}

	if err := m.RecordPatternObservation(ctx, "wsA", p); err != nil {
		t.Fatalf("opted-in capture: %v", err)
	}
	if err := m.RecordPatternObservation(ctx, "wsB", p); err != nil {
		t.Fatalf("non-opted-in capture: %v", err)
	}

	// wsA → exactly one row, opted_in=true, earned=0.
	var nA int
	var optedIn bool
	var earned float64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*), bool_and(opted_in), COALESCE(SUM(earned),0) FROM routing_patterns WHERE workspace_id='wsA'`).Scan(&nA, &optedIn, &earned); err != nil {
		t.Fatal(err)
	}
	if nA != 1 || !optedIn || earned != 0 {
		t.Fatalf("opted-in wsA: rows=%d opted_in=%v earned=%v, want 1/true/0", nA, optedIn, earned)
	}
	// wsB → NO row (opt-in WRITE gate).
	var nB int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_patterns WHERE workspace_id='wsB'`).Scan(&nB); err != nil {
		t.Fatal(err)
	}
	if nB != 0 {
		t.Fatalf("NON-opted-in wsB: rows=%d, want 0 (capture write gated on consent)", nB)
	}
	// Total earned across all capture rows is 0 — capture never credits.
	var totalEarned float64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(SUM(earned),0) FROM routing_patterns`).Scan(&totalEarned); err != nil {
		t.Fatal(err)
	}
	if totalEarned != 0 {
		t.Fatalf("capture must write earned=0 everywhere; total earned=%v", totalEarned)
	}
}
