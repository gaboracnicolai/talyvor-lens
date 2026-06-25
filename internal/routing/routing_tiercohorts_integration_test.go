package routing

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/mining"
)

// Real-PG end-to-end proof for tier-conditioned cohorts (Shape 3): a REAL *mining.PatternMiner
// (the production cohortSource) feeds a REAL Advisor, over real routing_patterns. Proves
// (a) tier-conditioning picks the right way, (c) the per-tier MinWorkspaces privacy floor excludes
// a near-individual 2-workspace slice, and (d) graceful tiered→non-tiered→default fallback.
// Gated on LENS_TEST_DATABASE_URL.

func tierHarness(t *testing.T) (*pgxpool.Pool, *Advisor) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG tier-cohort test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), `DROP TABLE IF EXISTS routing_patterns`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `CREATE TABLE routing_patterns (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL,
		feature_category TEXT NOT NULL, model_used TEXT NOT NULL, provider_used TEXT NOT NULL,
		input_token_range TEXT NOT NULL, output_quality DOUBLE PRECISION NOT NULL DEFAULT 0,
		latency_bucket TEXT NOT NULL DEFAULT '', cache_hit_rate DOUBLE PRECISION NOT NULL DEFAULT 0,
		success_rate DOUBLE PRECISION NOT NULL DEFAULT 1, sample_count INT NOT NULL DEFAULT 1,
		rarity DOUBLE PRECISION NOT NULL DEFAULT 0, complexity_bucket TEXT NOT NULL DEFAULT '',
		opted_in BOOLEAN NOT NULL DEFAULT FALSE, earned DOUBLE PRECISION NOT NULL DEFAULT 0,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	miner := mining.NewPatternMiner(nil, pool) // nil ledger — read-only aggregation, no mint reach
	adv := New(miner, costStub, Config{Enabled: true, TierCohorts: true, MinSamples: 3, MinWorkspaces: 3})
	return pool, adv
}

func seedTierPattern(t *testing.T, pool *pgxpool.Pool, ws, feature, inputRange, complexity, model, provider string, quality float64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO routing_patterns (workspace_id, feature_category, input_token_range, complexity_bucket, model_used, provider_used, output_quality, opted_in)
		 VALUES ($1,$2,$3,$4,$5,$6,$7, TRUE)`,
		ws, feature, inputRange, complexity, model, provider, quality); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// (a) Tier-conditioning picks the right way: simple work → the cheap model (best quality-per-dollar),
// complex work → the stronger model (the cheap model's quality collapses on complex). Same
// (feature, input_range), different model BY TIER.
func TestTierCohorts_PicksByComplexity_Integration(t *testing.T) {
	pool, adv := tierHarness(t)
	ctx := context.Background()
	for _, ws := range []string{"w1", "w2", "w3"} {
		// SIMPLE: cheap gpt-4o-mini ($0.02) at 0.90 → qpd 45 ; gpt-4o ($0.10) at 0.92 → qpd 9.2.
		seedTierPattern(t, pool, ws, "chat", "medium", "simple", "gpt-4o-mini", "openai", 0.90)
		seedTierPattern(t, pool, ws, "chat", "medium", "simple", "gpt-4o", "openai", 0.92)
		// COMPLEX: cheap gpt-4o-mini collapses to 0.10 → qpd 5 ; gpt-4o at 0.95 → qpd 9.5 (wins).
		seedTierPattern(t, pool, ws, "chat", "medium", "complex", "gpt-4o-mini", "openai", 0.10)
		seedTierPattern(t, pool, ws, "chat", "medium", "complex", "gpt-4o", "openai", 0.95)
	}
	if err := adv.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if rec := adv.Recommend(ctx, "wsX", "chat", 1000, "simple", "openai", nil, nil); rec.Model != "gpt-4o-mini" {
		t.Errorf("simple work → %q, want gpt-4o-mini (cheap model is best quality-per-dollar for simple)", rec.Model)
	}
	if rec := adv.Recommend(ctx, "wsX", "chat", 1000, "complex", "openai", nil, nil); rec.Model != "gpt-4o" {
		t.Errorf("complex work → %q, want gpt-4o (the stronger model wins once the cheap model's quality collapses)", rec.Model)
	}
}

// (c) CONDITION 1 — the privacy floor at the finer granularity: a tiered cohort with only 2 distinct
// workspaces NEVER surfaces (its near-individual quality is excluded by the per-tier
// COUNT(DISTINCT workspace_id) < MinWorkspaces); Recommend falls back to the non-tiered pick.
func TestTierCohorts_PrivacyFloor_TwoWorkspaceExcluded_Integration(t *testing.T) {
	pool, adv := tierHarness(t)
	ctx := context.Background()
	// COMPLEX tier: only 2 distinct workspaces, a DISTINCTIVE high-quality model that would win if it surfaced.
	seedTierPattern(t, pool, "w1", "chat", "medium", "complex", "secret-premium", "openai", 0.99)
	seedTierPattern(t, pool, "w2", "chat", "medium", "complex", "secret-premium", "openai", 0.99)
	// A NON-tiered-eligible cohort (chat, medium) with ≥3 distinct workspaces on a different model.
	seedTierPattern(t, pool, "w3", "chat", "medium", "simple", "gpt-4o-mini", "openai", 0.85)
	seedTierPattern(t, pool, "w4", "chat", "medium", "simple", "gpt-4o-mini", "openai", 0.85)
	seedTierPattern(t, pool, "w5", "chat", "medium", "simple", "gpt-4o-mini", "openai", 0.85)
	if err := adv.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	rec := adv.Recommend(ctx, "wsX", "chat", 1000, "complex", "openai", nil, nil)
	if rec.Model == "secret-premium" {
		t.Fatalf("PRIVACY BREACH: a 2-workspace tiered cohort surfaced (%q) — the per-tier MinWorkspaces floor must exclude it", rec.Model)
	}
	if rec.Model != "gpt-4o-mini" {
		t.Errorf("expected the non-tiered fallback (gpt-4o-mini), got %q", rec.Model)
	}
}

// (d) CONDITION 2 — graceful fallback: an ABSENT tiered cohort (warm-up) falls back to the non-tiered
// pick; an absent feature entirely is BasisNone (the proxy keeps the default) — never a garbage pick.
func TestTierCohorts_GracefulFallback_Integration(t *testing.T) {
	pool, adv := tierHarness(t)
	ctx := context.Background()
	for _, ws := range []string{"w1", "w2", "w3"} {
		seedTierPattern(t, pool, ws, "chat", "medium", "simple", "gpt-4o-mini", "openai", 0.90)
	}
	if err := adv.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	// No 'complex' tiered cohort exists → fall back to the non-tiered (chat, medium) pick.
	if rec := adv.Recommend(ctx, "wsX", "chat", 1000, "complex", "openai", nil, nil); rec.Basis == BasisNone || rec.Model != "gpt-4o-mini" {
		t.Errorf("absent tiered cohort must fall back to the non-tiered pick, got basis=%q model=%q", rec.Basis, rec.Model)
	}
	// Absent feature entirely → BasisNone (default), never arbitrary.
	if rec := adv.Recommend(ctx, "wsX", "unknown-feature", 1000, "complex", "openai", nil, nil); rec.Basis != BasisNone {
		t.Errorf("absent feature must be BasisNone (the proxy keeps the default), got %q", rec.Basis)
	}
}
