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
	// Rarity 0 → base × 1 = 0.001.
	got := PatternEarning(RoutingPattern{Rarity: 0})
	if got != 0.001 {
		t.Fatalf("expected 0.001, got %f", got)
	}
}

func TestPatternEarning_RarityMultiplier(t *testing.T) {
	// Rarity 0.5 → base × (1 + 0.5 × 4) = 0.001 × 3 = 0.003.
	got := PatternEarning(RoutingPattern{Rarity: 0.5})
	diff := got - 0.003
	if diff < -1e-9 || diff > 1e-9 {
		t.Fatalf("expected 0.003, got %f", got)
	}
}

func TestPatternEarning_UniqueBonus(t *testing.T) {
	// Rarity 1.0 → base × max + bonus = 0.001 × 5 + 0.010 = 0.015.
	got := PatternEarning(RoutingPattern{Rarity: 1.0})
	diff := got - 0.015
	if diff < -1e-9 || diff > 1e-9 {
		t.Fatalf("expected 0.015, got %f", got)
	}
}

func TestPatternEarning_BonusOnlyAboveThreshold(t *testing.T) {
	// Rarity 0.7 (exactly threshold) → no bonus.
	atThreshold := PatternEarning(RoutingPattern{Rarity: 0.7})
	expected := PatternBaseRate * (1.0 + 0.7*4.0) // = 0.001 × 3.8 = 0.0038
	diff := atThreshold - expected
	if diff < -1e-9 || diff > 1e-9 {
		t.Fatalf("expected %f at threshold (no bonus), got %f", expected, atThreshold)
	}

	// Just above threshold → bonus kicks in.
	above := PatternEarning(RoutingPattern{Rarity: 0.71})
	if above <= atThreshold {
		t.Fatalf("expected bonus above threshold, got %f vs %f", above, atThreshold)
	}
}

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
	// RECONCILED for the S1 rarity bound. Rarity scoring — feature_category
	// dropped from the key (5 args); n=0 OTHER workspaces is below the
	// corroboration floor → rarity FLOORS to 0.0 (was 1.0). feature_category
	// is still PERSISTED on the INSERT row (analytics), just not scored.
	mock.ExpectQuery("SELECT COUNT\\(DISTINCT workspace_id\\)").
		WithArgs("ws_opt", "claude", "anthropic", InputBucketMedium, LatencyFast).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(0))
	// INSERT pattern row — rarity 0.0 (floored), earned 0.001 (base, no bonus);
	// feature_category "code" still persisted.
	mock.ExpectQuery("INSERT INTO routing_patterns").
		WithArgs("ws_opt", "code", "claude", "anthropic", InputBucketMedium,
			0.85, LatencyFast, 0.0, 1.0, 1, 0.0, true, 0.001).
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).
			AddRow("p1", time.Now()))
	// Credit earning: 0.001 LENS (base rate — the floored case earns base only).
	expectCreditOrDebit(mock, "ws_opt", 0, 0, 0, 0.001, 0.001, 0.001, 0)

	pattern := RoutingPattern{
		FeatureCategory: "code", ModelUsed: "claude", ProviderUsed: "anthropic",
		InputTokenRange: InputBucketMedium, LatencyBucket: LatencyFast,
		OutputQuality: 0.85, CacheHitRate: 0.0, SuccessRate: 1.0, SampleCount: 1,
	}
	if err := miner.RecordPattern(context.Background(), "ws_opt", pattern, true); err != nil {
		t.Fatalf("RecordPattern: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestRecordPattern_SkipsCreditWhenNotOptedIn(t *testing.T) {
	miner, mock := newMockPatternMiner(t)
	// INSERT with opted_in=false + earned=0 — no rarity scoring,
	// no credit.
	mock.ExpectQuery("INSERT INTO routing_patterns").
		WithArgs("ws_off", "code", "claude", "anthropic", InputBucketMedium,
			0.85, LatencyFast, 0.0, 1.0, 1, 0.0, false, 0.0).
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).
			AddRow("p_off", time.Now()))

	pattern := RoutingPattern{
		FeatureCategory: "code", ModelUsed: "claude", ProviderUsed: "anthropic",
		InputTokenRange: InputBucketMedium, LatencyBucket: LatencyFast,
		OutputQuality: 0.85, CacheHitRate: 0.0, SuccessRate: 1.0, SampleCount: 1,
	}
	if err := miner.RecordPattern(context.Background(), "ws_off", pattern, false); err != nil {
		t.Fatalf("RecordPattern: %v", err)
	}
}

// ─── GetContribution ────────────────────────────

func TestGetContribution_ReturnsTotals(t *testing.T) {
	miner, mock := newMockPatternMiner(t)
	last := time.Now().UTC()
	mock.ExpectQuery("SELECT COUNT\\(\\*\\),").
		WithArgs("ws_c", UniqueRarityThreshold).
		WillReturnRows(pgxmock.NewRows([]string{"count", "uniq", "earned", "last"}).
			AddRow(125, 7, 0.85, last))
	c, err := miner.GetContribution(context.Background(), "ws_c")
	if err != nil {
		t.Fatalf("GetContribution: %v", err)
	}
	if c.PatternsShared != 125 || c.UniquePatterns != 7 || c.TotalEarned != 0.85 {
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

func TestPatternRates_KeysPresent(t *testing.T) {
	r := PatternRates()
	for _, k := range []string{
		"base_per_pattern", "rarity_multiplier_max",
		"unique_pattern_bonus", "unique_rarity_threshold",
	} {
		if _, ok := r[k]; !ok {
			t.Fatalf("missing rate key %q", k)
		}
	}
}
