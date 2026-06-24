package mining

import (
	"context"
	"math"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Real-PG proof for PatternMiner.AggregateCohorts (aggregateCohortsSQL) — the SQL the routing
// Advisor loads on its timer. Previously only string-asserted (aggregate_cohorts_test.go); this
// EXECUTES it against the real routing_patterns schema. The opt-out privacy exclusion is the
// point: it moves from a string check to a proven DB behavior. Test-only — proves existing
// behavior, changes nothing. Gated on LENS_TEST_DATABASE_URL.

func aggCohortsHarness(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG AggregateCohorts test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	// routing_patterns mirrors migration 0023 (latency_bucket gets a test default so seeds stay
	// minimal — AggregateCohorts never reads it). No dependent views, so DROP TABLE is safe.
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS routing_patterns`,
		`CREATE TABLE routing_patterns (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			workspace_id TEXT NOT NULL, feature_category TEXT NOT NULL, model_used TEXT NOT NULL,
			provider_used TEXT NOT NULL, input_token_range TEXT NOT NULL, output_quality DOUBLE PRECISION NOT NULL DEFAULT 0,
			latency_bucket TEXT NOT NULL DEFAULT '', cache_hit_rate DOUBLE PRECISION NOT NULL DEFAULT 0,
			success_rate DOUBLE PRECISION NOT NULL DEFAULT 1, sample_count INT NOT NULL DEFAULT 1,
			rarity DOUBLE PRECISION NOT NULL DEFAULT 0, opted_in BOOLEAN NOT NULL DEFAULT FALSE,
			earned DOUBLE PRECISION NOT NULL DEFAULT 0, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool
}

func seedRoutingPattern(t *testing.T, pool *pgxpool.Pool, ws, feature, inputRange, model, provider string, quality float64, optedIn bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO routing_patterns (workspace_id, feature_category, input_token_range, model_used, provider_used, output_quality, opted_in)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		ws, feature, inputRange, model, provider, quality, optedIn); err != nil {
		t.Fatalf("seed routing_pattern: %v", err)
	}
}

func findCohortStat(cohorts []CohortStat, feature, inputRange, model string) (CohortStat, bool) {
	for _, c := range cohorts {
		if c.FeatureCategory == feature && c.InputTokenRange == inputRange && c.ModelUsed == model {
			return c, true
		}
	}
	return CohortStat{}, false
}

// (1) PRIVACY EXCLUSION — opted_in=FALSE rows are excluded from counts AND from AVG; an
// all-opted-out cohort does not appear at all. (The property that was only string-asserted.)
func TestAggregateCohorts_PrivacyExclusion_Integration(t *testing.T) {
	pool := aggCohortsHarness(t)
	ctx := context.Background()
	m := NewPatternMiner(nil, pool)

	// SAME cohort (chat, medium, gpt-4o, openai): 3 opted-IN + 2 opted-OUT (low quality).
	seedRoutingPattern(t, pool, "wsA", "chat", "medium", "gpt-4o", "openai", 0.8, true)
	seedRoutingPattern(t, pool, "wsB", "chat", "medium", "gpt-4o", "openai", 0.9, true)
	seedRoutingPattern(t, pool, "wsC", "chat", "medium", "gpt-4o", "openai", 1.0, true)
	seedRoutingPattern(t, pool, "wsD", "chat", "medium", "gpt-4o", "openai", 0.1, false) // opted OUT
	seedRoutingPattern(t, pool, "wsE", "chat", "medium", "gpt-4o", "openai", 0.1, false) // opted OUT
	// An ENTIRELY opted-out cohort — must not surface at all.
	seedRoutingPattern(t, pool, "wsF", "vibe", "large", "claude", "anthropic", 0.5, false)
	seedRoutingPattern(t, pool, "wsG", "vibe", "large", "claude", "anthropic", 0.5, false)

	cohorts, err := m.AggregateCohorts(ctx)
	if err != nil {
		t.Fatal(err)
	}

	c, ok := findCohortStat(cohorts, "chat", "medium", "gpt-4o")
	if !ok {
		t.Fatal("opted-in cohort missing from aggregate")
	}
	// counts reflect ONLY the 3 opted-in rows — the 2 opted-out are excluded.
	if c.SampleCount != 3 {
		t.Errorf("SampleCount %d, want 3 — opted-out rows must be excluded from COUNT(*)", c.SampleCount)
	}
	if c.DistinctWorkspaces != 3 {
		t.Errorf("DistinctWorkspaces %d, want 3 — opted-out workspaces must be excluded", c.DistinctWorkspaces)
	}
	// AVG over only opted-in: (0.8+0.9+1.0)/3 = 0.9 — NOT pulled down by the 0.1 opted-out rows
	// (which would give (0.8+0.9+1.0+0.1+0.1)/5 = 0.58 if wrongly included).
	if math.Abs(c.AvgQuality-0.9) > 1e-9 {
		t.Errorf("AvgQuality %v, want 0.9 — opted-out 0.1 rows must not affect the average", c.AvgQuality)
	}
	// the all-opted-out cohort must be ABSENT.
	if _, present := findCohortStat(cohorts, "vibe", "large", "claude"); present {
		t.Error("an entirely opted-out cohort surfaced in the aggregate — privacy exclusion breach")
	}
}

// (2) AGGREGATION CORRECTNESS — AVG exact, SampleCount=COUNT(*), DistinctWorkspaces=COUNT(DISTINCT ws).
func TestAggregateCohorts_AggregationCorrectness_Integration(t *testing.T) {
	pool := aggCohortsHarness(t)
	ctx := context.Background()
	m := NewPatternMiner(nil, pool)

	// 4 opted-in rows across 2 distinct workspaces; qualities 0.6,0.8 (ws1) + 0.7,0.9 (ws2).
	seedRoutingPattern(t, pool, "ws1", "code", "small", "gpt-4o-mini", "openai", 0.6, true)
	seedRoutingPattern(t, pool, "ws1", "code", "small", "gpt-4o-mini", "openai", 0.8, true)
	seedRoutingPattern(t, pool, "ws2", "code", "small", "gpt-4o-mini", "openai", 0.7, true)
	seedRoutingPattern(t, pool, "ws2", "code", "small", "gpt-4o-mini", "openai", 0.9, true)

	cohorts, err := m.AggregateCohorts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := findCohortStat(cohorts, "code", "small", "gpt-4o-mini")
	if !ok {
		t.Fatal("cohort missing")
	}
	if c.SampleCount != 4 {
		t.Errorf("SampleCount %d, want 4 (COUNT(*))", c.SampleCount)
	}
	if c.DistinctWorkspaces != 2 {
		t.Errorf("DistinctWorkspaces %d, want 2 (COUNT(DISTINCT workspace_id), despite 4 rows)", c.DistinctWorkspaces)
	}
	if math.Abs(c.AvgQuality-0.75) > 1e-9 {
		t.Errorf("AvgQuality %v, want 0.75 ((0.6+0.7+0.8+0.9)/4)", c.AvgQuality)
	}
	if c.ProviderUsed != "openai" {
		t.Errorf("ProviderUsed %q, want openai", c.ProviderUsed)
	}
}

// (3) COHORT GROUPING — rows differing by ONE grouping key form SEPARATE cohorts (not merged).
func TestAggregateCohorts_GroupsDistinctCohortsSeparately_Integration(t *testing.T) {
	pool := aggCohortsHarness(t)
	ctx := context.Background()
	m := NewPatternMiner(nil, pool)

	seedRoutingPattern(t, pool, "ws1", "chat", "medium", "gpt-4o", "openai", 0.8, true)
	seedRoutingPattern(t, pool, "ws1", "chat", "medium", "gpt-4o-mini", "openai", 0.7, true) // diff model
	seedRoutingPattern(t, pool, "ws1", "chat", "large", "gpt-4o", "openai", 0.9, true)       // diff input_range
	seedRoutingPattern(t, pool, "ws1", "code", "medium", "gpt-4o", "openai", 0.6, true)      // diff feature

	cohorts, err := m.AggregateCohorts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cohorts) != 4 {
		t.Fatalf("got %d cohorts, want 4 distinct (must not merge): %+v", len(cohorts), cohorts)
	}
	for _, want := range []struct {
		f, r, mdl string
		q         float64
	}{
		{"chat", "medium", "gpt-4o", 0.8},
		{"chat", "medium", "gpt-4o-mini", 0.7},
		{"chat", "large", "gpt-4o", 0.9},
		{"code", "medium", "gpt-4o", 0.6},
	} {
		c, ok := findCohortStat(cohorts, want.f, want.r, want.mdl)
		if !ok {
			t.Errorf("cohort (%s,%s,%s) missing — wrongly merged?", want.f, want.r, want.mdl)
			continue
		}
		if c.SampleCount != 1 || math.Abs(c.AvgQuality-want.q) > 1e-9 {
			t.Errorf("cohort (%s,%s,%s): count %d avg %v, want count 1 / avg %v", want.f, want.r, want.mdl, c.SampleCount, c.AvgQuality, want.q)
		}
	}
}

// (4) FLOOR-RELEVANT SHAPE — a sub-floor cohort is returned with its real counts, so the
// Advisor's MinSamples=20 / MinWorkspaces=3 floors (unit-tested in routing_test.go) can gate it.
func TestAggregateCohorts_BelowFloorCountsRepresentable_Integration(t *testing.T) {
	pool := aggCohortsHarness(t)
	ctx := context.Background()
	m := NewPatternMiner(nil, pool)

	// thin cohort: 2 rows, 1 workspace — below both floors.
	seedRoutingPattern(t, pool, "solo", "chat", "medium", "gpt-4o", "openai", 0.95, true)
	seedRoutingPattern(t, pool, "solo", "chat", "medium", "gpt-4o", "openai", 0.95, true)

	cohorts, err := m.AggregateCohorts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := findCohortStat(cohorts, "chat", "medium", "gpt-4o")
	if !ok {
		t.Fatal("sub-floor cohort missing — AggregateCohorts returns ALL cohorts; the floor is the Advisor's job")
	}
	if c.SampleCount != 2 {
		t.Errorf("SampleCount %d, want 2 (sub-floor count returned faithfully)", c.SampleCount)
	}
	if c.DistinctWorkspaces != 1 {
		t.Errorf("DistinctWorkspaces %d, want 1 (sub-floor count returned faithfully)", c.DistinctWorkspaces)
	}
}
