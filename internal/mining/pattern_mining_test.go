package mining

import (
	"context"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func newMockPatternMiner(t *testing.T) (*PatternMiner, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	return NewPatternMiner(newLedgerStore(mock), mock), mock
}

// ─── ExtractPattern bucketing ────────────────────

func TestExtractPattern_InputBuckets(t *testing.T) {
	cases := []struct {
		tokens int
		bucket string
	}{
		{100, InputBucketSmall},
		{499, InputBucketSmall},
		{500, InputBucketMedium},
		{1999, InputBucketMedium},
		{2000, InputBucketLarge},
		{7999, InputBucketLarge},
		{8000, InputBucketXLarge},
		{100000, InputBucketXLarge},
	}
	for _, c := range cases {
		p := ExtractPattern("chat", "m", "anthropic", c.tokens, 0, 0.5, 500, false)
		if p.InputTokenRange != c.bucket {
			t.Fatalf("tokens=%d → expected %s, got %s", c.tokens, c.bucket, p.InputTokenRange)
		}
	}
}

func TestExtractPattern_LatencyBuckets(t *testing.T) {
	cases := []struct {
		ms     int64
		bucket string
	}{
		{500, LatencyFast},
		{999, LatencyFast},
		{1000, LatencyMedium},
		{2999, LatencyMedium},
		{3000, LatencySlow},
		{10000, LatencySlow},
	}
	for _, c := range cases {
		p := ExtractPattern("chat", "m", "anthropic", 100, 100, 0.5, c.ms, false)
		if p.LatencyBucket != c.bucket {
			t.Fatalf("ms=%d → expected %s, got %s", c.ms, c.bucket, p.LatencyBucket)
		}
	}
}

func TestExtractPattern_CacheHitFlagMapsToRate(t *testing.T) {
	hit := ExtractPattern("chat", "m", "p", 0, 0, 0.5, 500, true)
	if hit.CacheHitRate != 1.0 {
		t.Fatalf("cache hit should map to rate 1.0, got %f", hit.CacheHitRate)
	}
	miss := ExtractPattern("chat", "m", "p", 0, 0, 0.5, 500, false)
	if miss.CacheHitRate != 0.0 {
		t.Fatalf("cache miss should map to rate 0.0, got %f", miss.CacheHitRate)
	}
}

// ─── PatternEarning ──────────────────────────────

func TestPatternEarning_Base(t *testing.T) {
	// Rarity 0 → base × 1 = 0.001 LENS = micro(0.001) µLENS.
	got := PatternEarning(RoutingPattern{Rarity: 0})
	if got != micro(0.001) {
		t.Fatalf("expected %d, got %d µLENS", micro(0.001), got)
	}
}

func TestPatternEarning_RarityMultiplier(t *testing.T) {
	// Rarity 0.5 → base × (1 + 0.5 × 4) = 0.001 × 3 = 0.003 (exact in µLENS).
	got := PatternEarning(RoutingPattern{Rarity: 0.5})
	if got != micro(0.003) {
		t.Fatalf("expected %d, got %d µLENS", micro(0.003), got)
	}
}

// (TestPatternEarning_UniqueBonus + TestPatternEarning_BonusOnlyAboveThreshold
// removed: they tested the unique-pattern bonus, which was structurally
// unearnable post-S1 (rarity > 0.7 is unreachable) and has been deleted. Live
// earnings across the reachable range are pinned by
// TestPatternEarning_ReachableRange_NoBonus.)

// ─── ScoreRarity ─────────────────────────────────

func TestScoreRarity_FirstPattern(t *testing.T) {
	miner, mock := newMockPatternMiner(t)
	// RECONCILED for the S1 rarity bound: the rarity key no longer includes
	// feature_category (5 args, not 6), and a first-ever / uncorroborated
	// pattern (n=0 < EarnCorroborationFloor) now FLOORS to 0.0 — it used to
	// return 1.0 (the manufacturable "unique-pays-most" premium this bound
	// removes).
	mock.ExpectQuery("SELECT COUNT\\(DISTINCT workspace_id\\)").
		WithArgs("ws_a", "claude", "anthropic", InputBucketMedium, LatencyFast).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(0))
	rarity, err := miner.ScoreRarity(context.Background(), RoutingPattern{
		WorkspaceID: "ws_a", FeatureCategory: "code", ModelUsed: "claude",
		ProviderUsed: "anthropic", InputTokenRange: InputBucketMedium, LatencyBucket: LatencyFast,
	})
	if err != nil {
		t.Fatalf("ScoreRarity: %v", err)
	}
	if rarity != 0.0 {
		t.Fatalf("first-ever/uncorroborated pattern (n=0) must FLOOR to 0.0, got %f", rarity)
	}
}

func TestScoreRarity_Common(t *testing.T) {
	miner, mock := newMockPatternMiner(t)
	// RECONCILED: feature_category dropped from the rarity key (5 args). n=99
	// is well above the corroboration floor, so the common-pattern rarity is
	// unchanged (1/(1+99)=0.01).
	mock.ExpectQuery("SELECT COUNT\\(DISTINCT workspace_id\\)").
		WithArgs("ws_a", "gpt-4o", "openai", InputBucketSmall, LatencyMedium).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(99))
	rarity, _ := miner.ScoreRarity(context.Background(), RoutingPattern{
		WorkspaceID: "ws_a", FeatureCategory: "chat", ModelUsed: "gpt-4o",
		ProviderUsed: "openai", InputTokenRange: InputBucketSmall, LatencyBucket: LatencyMedium,
	})
	// 1 / (1 + 99) = 0.01
	if rarity > 0.011 || rarity < 0.009 {
		t.Fatalf("expected ~0.01 for very common pattern, got %f", rarity)
	}
}

// ─── RecordPattern ──────────────────────────────

func TestRecordPattern_CreditsOptedInWorkspace(t *testing.T) {
	miner, mock := newMockPatternMiner(t)
	// Rarity scoring — feature_category dropped from the key (5 args, S1); n=0
	// OTHER workspaces is below the corroboration floor → rarity FLOORS to 0.0
	// → earned = base 0.001. (rarity COUNT runs on the pool, BEFORE the tx.)
	mock.ExpectQuery("SELECT COUNT\\(DISTINCT workspace_id\\)").
		WithArgs("ws_opt", "claude", "anthropic", InputBucketMedium, LatencyFast).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(0))
	// RECONCILED for the S2 single-tx restructure: INSERT + credit + cap COUNT
	// now ride ONE tx. The CREDIT-EFFECT assertions are UNCHANGED (0.001 LENS to
	// ws_opt, same balance/earned); only the sequence gains Begin, the under-cap
	// COUNT, and Commit. feature_category "code" still persisted on the row.
	mock.ExpectBegin()
	// S3 claim-first (earning): RowsAffected 1 = claim taken (not a dup).
	mock.ExpectExec("INSERT INTO pattern_mine_credits").
		WithArgs("req1", "ws_opt", micro(0.001)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectQuery("INSERT INTO routing_patterns").
		WithArgs("ws_opt", "code", "claude", "anthropic", InputBucketMedium,
			0.85, LatencyFast, 0.0, 1.0, 1, 0.0, "", true, micro(0.001)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).
			AddRow("p1", time.Now()))
	expectApplyTx(mock, "ws_opt", 0, 0, 0, micro(0.001), micro(0.001), micro(0.001), 0) // credit 0.001 (unchanged effect)
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM routing_patterns").
		WithArgs("ws_opt", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(1))) // 1 ≤ default cap 50000
	mock.ExpectCommit()

	pattern := RoutingPattern{
		FeatureCategory: "code", ModelUsed: "claude", ProviderUsed: "anthropic",
		InputTokenRange: InputBucketMedium, LatencyBucket: LatencyFast,
		OutputQuality: 0.85, CacheHitRate: 0.0, SuccessRate: 1.0, SampleCount: 1,
	}
	if err := miner.RecordPattern(context.Background(), "ws_opt", pattern, true, "req1"); err != nil {
		t.Fatalf("RecordPattern: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestRecordPattern_SkipsCreditWhenNotOptedIn(t *testing.T) {
	miner, mock := newMockPatternMiner(t)
	// RECONCILED (S2): the persist-only path still wraps in the tx, but runs
	// ONLY the INSERT (opted_in=false, earned=0) — no rarity scoring, no credit,
	// no cap COUNT, no MintedTokens. Behavior-identical to before the restructure.
	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO routing_patterns").
		WithArgs("ws_off", "code", "claude", "anthropic", InputBucketMedium,
			0.85, LatencyFast, 0.0, 1.0, 1, 0.0, "", false, int64(0)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).
			AddRow("p_off", time.Now()))
	mock.ExpectCommit()

	pattern := RoutingPattern{
		FeatureCategory: "code", ModelUsed: "claude", ProviderUsed: "anthropic",
		InputTokenRange: InputBucketMedium, LatencyBucket: LatencyFast,
		OutputQuality: 0.85, CacheHitRate: 0.0, SuccessRate: 1.0, SampleCount: 1,
	}
	if err := miner.RecordPattern(context.Background(), "ws_off", pattern, false, "req-off"); err != nil {
		t.Fatalf("RecordPattern: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("not-opted-in must be INSERT-only in the tx (no credit, no cap): %v", err)
	}
}

// ─── GetContribution ────────────────────────────

func TestGetContribution_ReturnsTotals(t *testing.T) {
	miner, mock := newMockPatternMiner(t)
	last := time.Now().UTC()
	// RECONCILED: the dead unique-patterns FILTER column + its UniqueRarityThreshold
	// arg are gone (the metric was structurally always 0 post-S1).
	mock.ExpectQuery("SELECT COUNT\\(\\*\\),").
		WithArgs("ws_c").
		WillReturnRows(pgxmock.NewRows([]string{"count", "earned", "last"}).
			AddRow(125, micro(0.85), last))
	c, err := miner.GetContribution(context.Background(), "ws_c")
	if err != nil {
		t.Fatalf("GetContribution: %v", err)
	}
	if c.PatternsShared != 125 || c.TotalEarned != micro(0.85) {
		t.Fatalf("unexpected contribution: %+v", c)
	}
}

// ─── GetInsights ────────────────────────────────

func TestGetInsights_AggregatesAcrossWorkspaces(t *testing.T) {
	miner, mock := newMockPatternMiner(t)
	// 1. AvgQualityByInputRange — two buckets, sample counts 12 + 8 = 20.
	mock.ExpectQuery("SELECT input_token_range, AVG\\(output_quality\\), COUNT\\(\\*\\) FROM routing_patterns").
		WithArgs("code").
		WillReturnRows(pgxmock.NewRows([]string{"input_token_range", "avg", "count"}).
			AddRow(InputBucketMedium, 0.85, 12).
			AddRow(InputBucketLarge, 0.78, 8))
	// 2. CacheHitRateByFeature.
	mock.ExpectQuery("SELECT feature_category, AVG\\(cache_hit_rate\\)").
		WithArgs("code").
		WillReturnRows(pgxmock.NewRows([]string{"feature_category", "avg"}).
			AddRow("code", 0.42))
	// 3. RecommendedModel + BestQualityLatencyBucket.
	mock.ExpectQuery("SELECT model_used, latency_bucket FROM routing_patterns").
		WithArgs("code").
		WillReturnRows(pgxmock.NewRows([]string{"model", "bucket"}).
			AddRow("claude-haiku-4-5", LatencyFast))

	insights, err := miner.GetInsights(context.Background(), "", "", "code")
	if err != nil {
		t.Fatalf("GetInsights: %v", err)
	}
	if insights.SampleSize != 20 {
		t.Fatalf("expected SampleSize 20, got %d", insights.SampleSize)
	}
	if insights.AvgQualityByInputRange[InputBucketMedium] != 0.85 {
		t.Fatalf("expected medium quality 0.85, got %f", insights.AvgQualityByInputRange[InputBucketMedium])
	}
	if insights.CacheHitRateByFeature["code"] != 0.42 {
		t.Fatalf("expected code cache rate 0.42, got %f", insights.CacheHitRateByFeature["code"])
	}
	if insights.RecommendedModel != "claude-haiku-4-5" || insights.BestQualityLatencyBucket != LatencyFast {
		t.Fatalf("unexpected recommended: %+v", insights)
	}
}

// ─── OptIn / OptOut ─────────────────────────────

func TestOptInOptOut(t *testing.T) {
	miner, mock := newMockPatternMiner(t)
	mock.ExpectExec("INSERT INTO workspace_pattern_optin").
		WithArgs("ws_in").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	if err := miner.OptIn(context.Background(), "ws_in"); err != nil {
		t.Fatalf("OptIn: %v", err)
	}
	mock.ExpectExec("DELETE FROM workspace_pattern_optin").
		WithArgs("ws_in").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	if err := miner.OptOut(context.Background(), "ws_in"); err != nil {
		t.Fatalf("OptOut: %v", err)
	}
}

func TestIsOptedIn_ReturnsFalseWhenAbsent(t *testing.T) {
	miner, mock := newMockPatternMiner(t)
	mock.ExpectQuery("SELECT 1 FROM workspace_pattern_optin").
		WithArgs("ws_unknown").
		WillReturnError(errPgxNoRows)
	in, err := miner.IsOptedIn(context.Background(), "ws_unknown")
	if err != nil {
		t.Fatalf("IsOptedIn: %v", err)
	}
	if in {
		t.Fatal("expected not opted in")
	}
}

// ─── PatternRates ───────────────────────────────

func TestPatternRates_TruthfulPostS1(t *testing.T) {
	r := PatternRates()
	// Only EARNABLE economics are advertised. base_per_pattern is now µLENS (SEC-2).
	for _, k := range []string{"base_per_pattern_ulens", "rarity_multiplier_max"} {
		if _, ok := r[k]; !ok {
			t.Fatalf("missing rate key %q", k)
		}
	}
	// The dead unique-pattern bonus keys are GONE (pins the pre-freeze correction).
	for _, k := range []string{"unique_pattern_bonus", "unique_rarity_threshold"} {
		if _, ok := r[k]; ok {
			t.Fatalf("rate key %q must be REMOVED (the bonus is structurally unearnable post-S1)", k)
		}
	}
	// rarity_multiplier_max is the REACHABLE ceiling (2.0 at the corroboration
	// floor), not the unreachable 5×. PatternRates() is map[string]any now.
	if got, _ := r["rarity_multiplier_max"].(float64); got < 2.0-1e-9 || got > 2.0+1e-9 {
		t.Fatalf("rarity_multiplier_max must be the reachable 2.0, got %v (5.0 would advertise an unearnable ceiling)", r["rarity_multiplier_max"])
	}
	if got, _ := r["base_per_pattern_ulens"].(int64); got != PatternBaseRate {
		t.Fatalf("base_per_pattern_ulens = %v, want %d µLENS", r["base_per_pattern_ulens"], PatternBaseRate)
	}
}

// PatternEarning is byte-identical across the REACHABLE rarity range — {0.0}
// (uncorroborated, floored by ScoreRarity) ∪ (0, 0.25] (corroborated; rarity =
// 1/(1+n) for n≥EarnCorroborationFloor, max 1/(1+3)=0.25). The removed
// unique-pattern bonus fired only at rarity>0.7 (unreachable), so for EVERY
// input a workspace can actually produce, earnings are base × multiplier with
// NO bonus. This test passes BEFORE and AFTER the bonus-branch deletion — that
// invariance is the proof the deletion changes no live behavior. It also pins
// the truthful 2x reachable ceiling (not the old, unreachable 5x).
func TestPatternEarning_ReachableRange_NoBonus(t *testing.T) {
	maxReachable := 1.0 / (1.0 + float64(EarnCorroborationFloor)) // 0.25 at floor 3
	for _, r := range []float64{0.0, 0.05, 0.1, 1.0 / 6.0, 0.2, maxReachable} {
		got := PatternEarning(RoutingPattern{Rarity: r})
		want := MulFloor(PatternBaseRate, 1.0+r*(RarityMultiplierMax-1.0)) // µLENS, base × mult, NO bonus
		if got != want {                                                   // integer µLENS — exact
			t.Errorf("rarity %v: PatternEarning=%d, want %d µLENS (base × multiplier, no bonus)", r, got, want)
		}
	}
	if maxEarn := PatternEarning(RoutingPattern{Rarity: maxReachable}); maxEarn > micro(0.002) {
		t.Errorf("reachable max earn must be ≤ 0.002 LENS (2× ceiling, not the old 5×); got %d µLENS", maxEarn)
	}
}
