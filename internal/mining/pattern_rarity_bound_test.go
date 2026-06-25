package mining

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pashagolub/pgxmock/v4"
)

// S1 RARITY BOUND — the floor matrix. rarity is computed over PROXY-SET dims
// only (feature_category excluded from the key), and floors to 0.0 below the
// cross-workspace corroboration floor (EarnCorroborationFloor=3 OTHER opted-in
// workspaces). The perverse n=0 unique-pattern case floors instead of maxing.
//
// The WithArgs here OMIT feature_category (5 args, not 6) — proving (b): the
// rarity key no longer includes the caller-controlled field. If ScoreRarity
// still passed feature, the live query would carry 6 args and pgxmock would not
// match.
func TestScoreRarity_FloorMatrix(t *testing.T) {
	cases := []struct {
		name   string
		n      int
		expect float64
	}{
		{"n=0 unique (the perverse case) floors", 0, 0.0},
		{"n=1 thin floors", 1, 0.0},
		{"n=2 still below floor", 2, 0.0},
		{"n=3 at floor → 1/(1+3)", 3, 0.25},
		{"n=9 corroborated → 1/(1+9)", 9, 0.1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			miner, mock := newMockPatternMiner(t)
			mock.ExpectQuery("SELECT COUNT\\(DISTINCT workspace_id\\)").
				WithArgs("ws_a", "claude", "anthropic", InputBucketMedium, LatencyFast). // NO feature arg
				WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(c.n))
			rarity, err := miner.ScoreRarity(context.Background(), RoutingPattern{
				WorkspaceID: "ws_a", FeatureCategory: "anything-the-caller-wants",
				ModelUsed: "claude", ProviderUsed: "anthropic",
				InputTokenRange: InputBucketMedium, LatencyBucket: LatencyFast,
			})
			if err != nil {
				t.Fatalf("ScoreRarity: %v", err)
			}
			if d := rarity - c.expect; d < -1e-9 || d > 1e-9 {
				t.Fatalf("n=%d: rarity=%v, want %v", c.n, rarity, c.expect)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("feature must NOT be in the rarity-key args: %v", err)
			}
		})
	}
}

// nil pool → FAIL-CLOSED (0.0): if corroboration can't be verified, do NOT pay
// the premium. (Was 1.0 — the old "first pattern is maximally rare" behavior.)
func TestScoreRarity_NilPool_FailsClosed(t *testing.T) {
	m := NewPatternMiner(nil, nil)
	rarity, err := m.ScoreRarity(context.Background(), RoutingPattern{WorkspaceID: "ws"})
	if err != nil {
		t.Fatalf("ScoreRarity nil pool: %v", err)
	}
	if rarity != 0.0 {
		t.Fatalf("nil pool must FAIL CLOSED to 0.0 (no corroboration ⇒ no premium); got %v", rarity)
	}
}

// The floored case earns BASE only (no bonus): PatternEarning(rarity=0)=0.001.
func TestPatternEarning_FlooredRarity_BaseNoBonus(t *testing.T) {
	got := PatternEarning(RoutingPattern{Rarity: 0.0})
	if d := got - PatternBaseRate; d < -1e-9 || d > 1e-9 {
		t.Fatalf("floored rarity must earn base %v (no bonus); got %v", PatternBaseRate, got)
	}
}

// THE MAKE-OR-BREAK (real-PG): feature variance does NOT change rarity, and a
// unique proxy-set tuple floors. Three OTHER workspaces share a proxy-set tuple
// (each with a DIFFERENT feature string) → the earner scoring the same proxy-set
// tuple sees n=3 regardless of its own feature → rarity 0.25. A workspace whose
// proxy-set tuple nobody else shares sees n=0 → floored 0.0.
func TestScoreRarity_FeatureExcluded_Integration(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG rarity-bound test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS routing_patterns`,
		`CREATE TABLE routing_patterns (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			workspace_id TEXT NOT NULL, feature_category TEXT NOT NULL, model_used TEXT NOT NULL,
			provider_used TEXT NOT NULL, input_token_range TEXT NOT NULL, output_quality DOUBLE PRECISION NOT NULL DEFAULT 0,
			latency_bucket TEXT NOT NULL, cache_hit_rate DOUBLE PRECISION NOT NULL DEFAULT 0,
			success_rate DOUBLE PRECISION NOT NULL DEFAULT 1, sample_count INT NOT NULL DEFAULT 1,
			rarity DOUBLE PRECISION NOT NULL DEFAULT 0, complexity_bucket TEXT NOT NULL DEFAULT '', opted_in BOOLEAN NOT NULL DEFAULT FALSE,
			earned DOUBLE PRECISION NOT NULL DEFAULT 0, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		// 3 OTHER opted-in workspaces, SAME proxy-set tuple, DIFFERENT feature each.
		`INSERT INTO routing_patterns (workspace_id, feature_category, model_used, provider_used, input_token_range, latency_bucket, opted_in)
		 VALUES ('ws_1','feat-A','claude','anthropic','medium','fast',TRUE),
		        ('ws_2','feat-B','claude','anthropic','medium','fast',TRUE),
		        ('ws_3','feat-C','claude','anthropic','medium','fast',TRUE)`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	m := NewPatternMiner(nil, pool)

	// Earner ws_x, SAME proxy-set tuple, a UNIQUE feature string nobody else used.
	// feature is excluded from the key → n=3 (the 3 others) → rarity 0.25.
	r, err := m.ScoreRarity(ctx, RoutingPattern{
		WorkspaceID: "ws_x", FeatureCategory: "totally-unique-feature-xyz",
		ModelUsed: "claude", ProviderUsed: "anthropic", InputTokenRange: "medium", LatencyBucket: "fast",
	})
	if err != nil {
		t.Fatal(err)
	}
	if d := r - 0.25; d < -1e-9 || d > 1e-9 {
		t.Fatalf("unique feature must NOT change rarity (feature excluded from key); n=3 → want 0.25, got %v", r)
	}

	// A unique PROXY-SET tuple (different model) nobody else shares → n=0 → floored.
	r0, err := m.ScoreRarity(ctx, RoutingPattern{
		WorkspaceID: "ws_x", FeatureCategory: "feat-A",
		ModelUsed: "gpt-4o", ProviderUsed: "openai", InputTokenRange: "medium", LatencyBucket: "fast",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r0 != 0.0 {
		t.Fatalf("a proxy-set tuple no other workspace shares (n=0) must floor to 0.0; got %v", r0)
	}
}
