package mining

// pattern_mining.go — fifth and most novel LENS mining track.
// Workspaces that opt in to sharing their anonymised routing
// patterns earn LENS proportional to how rare/valuable those
// patterns turn out to be.
//
// Privacy model:
//   - Raw prompts + responses are never touched here.
//   - Only bucketed categorical signals make it into the row
//     (feature category, model, provider, input-token bucket,
//     latency bucket, quality score, cache hit rate).
//   - Rarity is computed at INSERT time against the existing
//     corpus of opted-in patterns from OTHER workspaces.
//
// Two opt-in gates must both be on for earnings:
//   1. The deployment operator sets LENS_PATTERN_MINING_ENABLED=true.
//   2. The workspace explicitly opts in via /v1/workspaces/:wsID/pattern-mining/opt-in.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/metrics"
)

// ─── constants ───────────────────────────────────

const (
	// PatternBaseRate is the LENS floor for one shared pattern.
	// Rarity multiplier stacks on top of this.
	PatternBaseRate = 0.001

	// RarityMultiplierMax is the highest the rarity multiplier
	// can climb. A perfectly unique pattern earns
	// PatternBaseRate × RarityMultiplierMax + UniquePatternBonus.
	RarityMultiplierMax = 5.0

	// UniquePatternBonus stacks on top when rarity > 0.7 —
	// rewards "truly novel" patterns disproportionately.
	UniquePatternBonus = 0.010

	// UniqueRarityThreshold is the rarity floor at which the
	// bonus fires.
	UniqueRarityThreshold = 0.7

	// EarnCorroborationFloor is the minimum number of OTHER opted-in
	// workspaces that must independently produce the same proxy-set tuple
	// before rarity earns above the base rate. Below it (including the
	// perverse n=0 unique-pattern case) ScoreRarity returns 0.0 — base rate,
	// no premium, no bonus. This is the anti-gaming bound: "unique-pays-most"
	// rewarded exactly the unverifiable/manufacturable patterns; corroboration
	// by independent workspaces is now required to earn a premium. Mirrors the
	// routing Advisor's read-side floor (routing.defaultMinWorkspaces = 3);
	// kept as a separate mining-side constant to avoid cross-package coupling.
	EarnCorroborationFloor = 3

	// TypePatternMine is the ledger row type for this track.
	TypePatternMine = "pattern_mine"
)

// Input-token bucket boundaries — kept as constants so callers
// (and tests) can refer to them by name.
const (
	InputBucketSmall  = "small"
	InputBucketMedium = "medium"
	InputBucketLarge  = "large"
	InputBucketXLarge = "xlarge"
)

const (
	LatencyFast   = "fast"
	LatencyMedium = "medium"
	LatencySlow   = "slow"
)

// ─── types ───────────────────────────────────────

// RoutingPattern is the shape persisted to routing_patterns.
type RoutingPattern struct {
	ID              string    `json:"id"`
	WorkspaceID     string    `json:"workspace_id"`
	FeatureCategory string    `json:"feature_category"`
	ModelUsed       string    `json:"model_used"`
	ProviderUsed    string    `json:"provider_used"`
	InputTokenRange string    `json:"input_token_range"`
	OutputQuality   float64   `json:"output_quality"`
	LatencyBucket   string    `json:"latency_bucket"`
	CacheHitRate    float64   `json:"cache_hit_rate"`
	SuccessRate     float64   `json:"success_rate"`
	SampleCount     int       `json:"sample_count"`
	Rarity          float64   `json:"rarity"`
	CreatedAt       time.Time `json:"created_at"`
}

// PatternContribution is the per-workspace rollup the dashboard
// endpoint returns.
type PatternContribution struct {
	WorkspaceID    string    `json:"workspace_id"`
	PatternsShared int       `json:"patterns_shared"`
	UniquePatterns int       `json:"unique_patterns"`
	TotalEarned    float64   `json:"total_earned"`
	LastSharedAt   time.Time `json:"last_shared_at,omitempty"`
}

// PatternInsights is the aggregated, anonymised payload the
// public /v1/insights/routing endpoint serves.
type PatternInsights struct {
	BestQualityLatencyBucket string             `json:"best_quality_latency_bucket,omitempty"`
	AvgQualityByInputRange   map[string]float64 `json:"avg_quality_by_input_range"`
	CacheHitRateByFeature    map[string]float64 `json:"cache_hit_rate_by_feature"`
	RecommendedModel         string             `json:"recommended_model,omitempty"`
	SampleSize               int                `json:"sample_size"`
}

// ─── bucketing helpers ───────────────────────────

// inputBucket categorises raw input-token counts into the four
// privacy-preserving buckets the spec defines.
func inputBucket(tokens int) string {
	switch {
	case tokens < 500:
		return InputBucketSmall
	case tokens < 2000:
		return InputBucketMedium
	case tokens < 8000:
		return InputBucketLarge
	default:
		return InputBucketXLarge
	}
}

// InputBucketFor exposes the privacy-preserving input-token bucketing so
// consumers of the aggregated corpus (the routing advisor, Upgrade 22)
// bucket a request's input size identically to how patterns were recorded —
// rather than duplicating the boundaries.
func InputBucketFor(tokens int) string { return inputBucket(tokens) }

// latencyBucket categorises raw latency into the three buckets.
func latencyBucket(latencyMs int64) string {
	switch {
	case latencyMs < 1000:
		return LatencyFast
	case latencyMs < 3000:
		return LatencyMedium
	default:
		return LatencySlow
	}
}

// ExtractPattern is the privacy-layer constructor. Callers
// pass raw values; we return only the bucketed pattern shape
// the database actually stores. Quality + cache-hit-rate flow
// through unchanged (already 0-1 scalars).
func ExtractPattern(
	feature, model, provider string,
	inputTokens, outputTokens int,
	quality float64,
	latencyMs int64,
	cacheHit bool,
) RoutingPattern {
	hitRate := 0.0
	if cacheHit {
		hitRate = 1.0
	}
	return RoutingPattern{
		FeatureCategory: feature,
		ModelUsed:       model,
		ProviderUsed:    provider,
		InputTokenRange: inputBucket(inputTokens),
		LatencyBucket:   latencyBucket(latencyMs),
		OutputQuality:   quality,
		CacheHitRate:    hitRate,
		SuccessRate:     1.0,
		SampleCount:     1,
		CreatedAt:       time.Now(),
	}
}

// ─── earning calculator ──────────────────────────

// PatternEarning computes the LENS payout for a pattern.
//
//	base × (1 + rarity × (max - 1)) [+ bonus if rare]
//
// Rounded to 6 decimals like the other mining tracks.
func PatternEarning(p RoutingPattern) float64 {
	if p.Rarity < 0 {
		p.Rarity = 0
	}
	if p.Rarity > 1 {
		p.Rarity = 1
	}
	mult := 1.0 + p.Rarity*(RarityMultiplierMax-1.0)
	earning := PatternBaseRate * mult
	if p.Rarity > UniqueRarityThreshold {
		earning += UniquePatternBonus
	}
	return roundTo(earning, 6)
}

// ─── PatternMiner ────────────────────────────────

// PatternMiner is the persistence + earning engine. Workspace
// opt-in is enforced at the API boundary (OptIn / OptOut +
// IsOptedIn); the per-call `optedIn` boolean on RecordPattern
// gives callers a final override that mirrors the env-level
// LENS_PATTERN_MINING_ENABLED gate.
type PatternMiner struct {
	ledger *LedgerStore
	pool   pgxDB

	// S2 per-window EARN cap (event count per workspace). Defaulted to a REAL
	// limit in NewPatternMiner so no miner is uncapped by accident — only a
	// test can opt out (SetEarnCap(0, …)). Inert until S4 wires the earn path:
	// RecordPattern has no production caller yet, so the cap never runs live.
	earnCapPerWorkspace int
	earnCapWindow       time.Duration
}

// DefaultPatternEarnCapPerWorkspace bounds manufactured-volume abuse of the
// self-generated earn path. Anchored to the ATTACK ceiling: with S1's
// corroboration floor an attacker cannot self-manufacture the premium, so a
// pure-volume attacker earns only the base rate — 50,000 events × 0.001 LENS =
// ~50 LENS/workspace/24h max. (A legitimately cross-workspace-corroborated
// workspace tops out near ~100 LENS/24h — the system rewarding real activity.)
// 50k sits >10× above plausible legitimate single-workspace qualifying volume,
// so it never bites organic use: catastrophe-prevention, not fine-tuning.
const DefaultPatternEarnCapPerWorkspace = 50_000

func NewPatternMiner(ledger *LedgerStore, pool pgxDB) *PatternMiner {
	return &PatternMiner{
		ledger:              ledger,
		pool:                pool,
		earnCapPerWorkspace: DefaultPatternEarnCapPerWorkspace,
		earnCapWindow:       24 * time.Hour,
	}
}

// SetEarnCap overrides the per-window earn cap (events per workspace) from
// config. perWorkspace = 0 is an explicit ops-disable (NOT the default). A
// non-positive window keeps the existing window.
func (m *PatternMiner) SetEarnCap(perWorkspace int, window time.Duration) {
	m.earnCapPerWorkspace = perWorkspace
	if window > 0 {
		m.earnCapWindow = window
	}
}

// OptIn flips the workspace flag — patterns recorded after this
// land with opted_in=true and the workspace earns LENS for them.
func (m *PatternMiner) OptIn(ctx context.Context, workspaceID string) error {
	if m.pool == nil {
		return nil
	}
	_, err := m.pool.Exec(ctx, `
		INSERT INTO workspace_pattern_optin (workspace_id)
		VALUES ($1)
		ON CONFLICT (workspace_id) DO UPDATE SET opted_in_at = NOW()`,
		workspaceID)
	if err != nil {
		return fmt.Errorf("pattern mining: opt-in: %w", err)
	}
	return nil
}

// OptOut removes the workspace flag. Already-recorded patterns
// keep their opted_in=true status (they're already part of the
// collective intelligence corpus) — opting out only stops
// future recording.
func (m *PatternMiner) OptOut(ctx context.Context, workspaceID string) error {
	if m.pool == nil {
		return nil
	}
	_, err := m.pool.Exec(ctx,
		`DELETE FROM workspace_pattern_optin WHERE workspace_id = $1`, workspaceID)
	if err != nil {
		return fmt.Errorf("pattern mining: opt-out: %w", err)
	}
	return nil
}

// IsOptedIn reports whether the workspace has the toggle on.
func (m *PatternMiner) IsOptedIn(ctx context.Context, workspaceID string) (bool, error) {
	if m.pool == nil {
		return false, nil
	}
	row := m.pool.QueryRow(ctx,
		`SELECT 1 FROM workspace_pattern_optin WHERE workspace_id = $1`, workspaceID)
	var n int
	if err := row.Scan(&n); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("pattern mining: read opt-in: %w", err)
	}
	return true, nil
}

// ─── rarity scoring ──────────────────────────────

// ScoreRarity counts how many OTHER opted-in workspaces have submitted a
// pattern with the same PROXY-SET tuple (model, provider, input_range,
// latency_bucket) — feature_category is deliberately NOT in the key (see the
// body). Below EarnCorroborationFloor corroborating workspaces (including the
// first-ever / n=0 case) it returns 0.0; at or above the floor it returns
// 1/(1+n). An uncorroborated or unverifiable pattern earns no premium.
//
//	rarity = 0.0                              if n < EarnCorroborationFloor
//	rarity = 1 / (1 + count_other_workspaces) otherwise
func (m *PatternMiner) ScoreRarity(ctx context.Context, p RoutingPattern) (float64, error) {
	if m.pool == nil {
		// FAIL-CLOSED: corroboration can't be verified without the DB, so do
		// NOT pay the premium (was 1.0 — the old "first pattern is maximally
		// rare" behavior, which is exactly the manufacturable case this bound
		// removes).
		return 0.0, nil
	}
	// (b) The rarity key is PROXY-SET dimensions ONLY — model_used, provider_used,
	// input_token_range, latency_bucket. feature_category is DELIBERATELY excluded:
	// it is the one caller-controlled field (the X-Talyvor-Feature header), so
	// keying rarity on it would let a workspace manufacture uniqueness by varying
	// the header. feature_category is still PERSISTED on the row (analytics) — it
	// just cannot move the score.
	row := m.pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT workspace_id)
		FROM routing_patterns
		WHERE opted_in = TRUE
		  AND workspace_id <> $1
		  AND model_used = $2
		  AND provider_used = $3
		  AND input_token_range = $4
		  AND latency_bucket = $5`,
		p.WorkspaceID, p.ModelUsed, p.ProviderUsed,
		p.InputTokenRange, p.LatencyBucket)
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("pattern mining: rarity: %w", err)
	}
	// (c) CROSS-WORKSPACE CORROBORATION FLOOR. n counts OTHER opted-in workspaces
	// that independently produced the same proxy-set tuple (the earner does NOT
	// corroborate its own pattern). Below the floor — including the perverse n=0
	// unique case — rarity is 0.0, so PatternEarning yields the base rate with no
	// bonus. A pattern must be corroborated by enough OTHER workspaces to earn a
	// premium; "unique-pays-most" is gone. Mirrors the routing Advisor's read-side
	// MinWorkspaces floor (see EarnCorroborationFloor).
	if n < EarnCorroborationFloor {
		return 0.0, nil
	}
	return 1.0 / (1.0 + float64(n)), nil
}

// ─── RecordPattern ───────────────────────────────

// RecordPattern persists a pattern + (when opted-in) credits
// the workspace. `optedIn` is the AND of LENS_PATTERN_MINING_ENABLED
// (deployment-level) and IsOptedIn (workspace-level) — callers
// compute that AND once and pass it in.
//
// When optedIn is false we still INSERT the row (with
// opted_in=false) so the workspace can inspect their own
// pattern history; we just don't compute rarity or credit
// LENS.
// earnCapCountSQL is the S2 per-window earn cap — a per-WORKSPACE event count,
// reusing Pool-B's capCountSQL shape (COUNT over a microsecond window) but over
// routing_patterns. `earned > 0` counts only actual earn events, excluding the
// mint-free capture rows (RecordPatternObservation writes earned=0). Run INSIDE
// the mint tx AFTER CreditTx, so it rides the lens_token_balances FOR UPDATE
// that serializes same-workspace credits — exact under concurrency, zero new
// locks (the same trick Pool-B uses). Rides idx_patterns_workspace
// (workspace_id, created_at DESC); earned>0 is a cheap residual over one
// workspace's window — no new index, no migration. The count includes the
// just-inserted in-tx row, so n > cap means "this would be the (cap+1)th".
const earnCapCountSQL = `SELECT COUNT(*) FROM routing_patterns
WHERE workspace_id = $1 AND earned > 0
  AND created_at > now() - ($2::bigint * interval '1 microsecond')`

func (m *PatternMiner) RecordPattern(ctx context.Context, workspaceID string, p RoutingPattern, optedIn bool) error {
	p.WorkspaceID = workspaceID

	earned := 0.0
	if optedIn {
		rarity, err := m.ScoreRarity(ctx, p)
		if err != nil {
			// Best-effort: rarity scoring failure shouldn't drop
			// the pattern entirely. Continue with rarity=0 and
			// the floor payout.
			rarity = 0
		}
		p.Rarity = rarity
		earned = PatternEarning(p)
	}

	if m.pool == nil {
		return nil // no DB — no-op (tests that skip the DB path)
	}

	// SINGLE TX (S2): the routing_patterns INSERT, the credit, and the cap
	// COUNT all ride one tx with a deferred rollback, so an over-cap event
	// discards the row AND the credit atomically, and the cap COUNT rides
	// CreditTx's balance FOR UPDATE for exactness under concurrent
	// same-workspace bursts (mirrors poolroyalty.MintServedHit).
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pattern mining: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row := tx.QueryRow(ctx, `
		INSERT INTO routing_patterns (
			workspace_id, feature_category, model_used, provider_used,
			input_token_range, output_quality, latency_bucket,
			cache_hit_rate, success_rate, sample_count, rarity,
			opted_in, earned
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING id, created_at`,
		workspaceID, p.FeatureCategory, p.ModelUsed, p.ProviderUsed,
		p.InputTokenRange, p.OutputQuality, p.LatencyBucket,
		p.CacheHitRate, p.SuccessRate, p.SampleCount, p.Rarity,
		optedIn, earned,
	)
	if err := row.Scan(&p.ID, &p.CreatedAt); err != nil {
		return fmt.Errorf("pattern mining: insert pattern: %w", err)
	}

	// Not earning (not opted in, or no positive earning) → persist only.
	if !optedIn || earned <= 0 {
		return tx.Commit(ctx)
	}

	meta := map[string]interface{}{
		"pattern_id":        p.ID,
		"feature_category":  p.FeatureCategory,
		"model_used":        p.ModelUsed,
		"provider_used":     p.ProviderUsed,
		"input_token_range": p.InputTokenRange,
		"latency_bucket":    p.LatencyBucket,
		"rarity":            p.Rarity,
	}
	// Credit IN-TX (CreditTx, not Credit) — identical balance effect, but it
	// takes the lens_token_balances FOR UPDATE that the cap COUNT below rides.
	if err := m.ledger.CreditTx(ctx, tx, workspaceID, earned, TypePatternMine,
		"pattern shared", meta); err != nil {
		return fmt.Errorf("pattern mining: credit: %w", err)
	}

	// Per-window earn cap. Over cap → return nil WITHOUT commit: the deferred
	// rollback discards the row AND the credit atomically (serve-but-skip,
	// mirroring Pool-B's Result{Capped}). Cap-count error → return err before
	// commit → rollback → FAIL CLOSED (no credit when we can't verify the cap).
	if m.earnCapPerWorkspace > 0 {
		var n int64
		if err := tx.QueryRow(ctx, earnCapCountSQL,
			workspaceID, m.earnCapWindow.Microseconds()).Scan(&n); err != nil {
			return fmt.Errorf("pattern mining: earn cap count: %w", err)
		}
		if n > int64(m.earnCapPerWorkspace) {
			return nil // over cap — deferred rollback discards row+credit
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pattern mining: commit: %w", err)
	}
	// MintedTokens AFTER commit only — fires on a durable credit, never on a
	// capped/rolled-back one (CreditTx, unlike Credit, doesn't emit it).
	metrics.MintedTokens(earned)
	return nil
}

// ─── GetContribution ─────────────────────────────

// GetContribution rolls up the workspace's total pattern share
// + unique-pattern count + earnings.
func (m *PatternMiner) GetContribution(ctx context.Context, workspaceID string) (*PatternContribution, error) {
	c := &PatternContribution{WorkspaceID: workspaceID}
	if m.pool == nil {
		return c, nil
	}
	row := m.pool.QueryRow(ctx, `
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE rarity > $2),
		       COALESCE(SUM(earned), 0),
		       COALESCE(MAX(created_at), '1970-01-01'::timestamptz)
		FROM routing_patterns
		WHERE workspace_id = $1 AND opted_in = TRUE`,
		workspaceID, UniqueRarityThreshold)
	if err := row.Scan(&c.PatternsShared, &c.UniquePatterns, &c.TotalEarned, &c.LastSharedAt); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("pattern mining: contribution: %w", err)
		}
	}
	return c, nil
}

// ─── GetInsights ─────────────────────────────────

// GetInsights aggregates patterns from ALL opted-in workspaces
// (never per-workspace) and returns the privacy-safe rollup the
// public /v1/insights/routing endpoint exposes.
//
// `model`, `provider`, and `feature` are optional filters —
// pass empty strings to skip.
func (m *PatternMiner) GetInsights(ctx context.Context, model, provider, feature string) (*PatternInsights, error) {
	out := &PatternInsights{
		AvgQualityByInputRange: map[string]float64{},
		CacheHitRateByFeature:  map[string]float64{},
	}
	if m.pool == nil {
		return out, nil
	}

	// Build WHERE filter dynamically — string concatenation
	// is safe here because the filters are sourced from
	// trusted constant input fields (we don't pipe user-typed
	// model names directly to SQL).
	filters := "opted_in = TRUE"
	args := []any{}
	addArg := func(col string, val string) {
		if val == "" {
			return
		}
		args = append(args, val)
		filters += fmt.Sprintf(" AND %s = $%d", col, len(args))
	}
	addArg("feature_category", feature)
	addArg("model_used", model)
	addArg("provider_used", provider)

	// 1. AvgQualityByInputRange.
	rows, err := m.pool.Query(ctx,
		"SELECT input_token_range, AVG(output_quality), COUNT(*) FROM routing_patterns WHERE "+filters+" GROUP BY input_token_range", args...)
	if err != nil {
		return nil, fmt.Errorf("pattern mining: insights/range: %w", err)
	}
	var totalSamples int
	for rows.Next() {
		var bucket string
		var avg float64
		var n int
		if err := rows.Scan(&bucket, &avg, &n); err != nil {
			rows.Close()
			return nil, fmt.Errorf("pattern mining: scan range: %w", err)
		}
		out.AvgQualityByInputRange[bucket] = avg
		totalSamples += n
	}
	rows.Close()
	out.SampleSize = totalSamples

	// 2. CacheHitRateByFeature.
	rows, err = m.pool.Query(ctx,
		"SELECT feature_category, AVG(cache_hit_rate) FROM routing_patterns WHERE "+filters+" GROUP BY feature_category", args...)
	if err != nil {
		return nil, fmt.Errorf("pattern mining: insights/cache: %w", err)
	}
	for rows.Next() {
		var feat string
		var avg float64
		if err := rows.Scan(&feat, &avg); err != nil {
			rows.Close()
			return nil, fmt.Errorf("pattern mining: scan cache: %w", err)
		}
		out.CacheHitRateByFeature[feat] = avg
	}
	rows.Close()

	// 3. RecommendedModel + BestQualityLatencyBucket — pick the
	//    (model, latency_bucket) combo with the highest mean
	//    quality across the filtered set.
	row := m.pool.QueryRow(ctx,
		"SELECT model_used, latency_bucket FROM routing_patterns WHERE "+filters+
			" GROUP BY model_used, latency_bucket ORDER BY AVG(output_quality) DESC LIMIT 1", args...)
	var bestModel, bestBucket string
	if err := row.Scan(&bestModel, &bestBucket); err == nil {
		out.RecommendedModel = bestModel
		out.BestQualityLatencyBucket = bestBucket
	}

	return out, nil
}

// ─── GetTopInsight ───────────────────────────────

// GetTopInsight returns the single best (highest avg quality)
// pattern row for (feature, input_range) across all opted-in
// workspaces. Used by the smart router to pick a model for a
// new request shape.
func (m *PatternMiner) GetTopInsight(ctx context.Context, feature, inputRange string) (*RoutingPattern, error) {
	if m.pool == nil {
		return nil, nil
	}
	row := m.pool.QueryRow(ctx, `
		SELECT model_used, provider_used, input_token_range,
		       AVG(output_quality), latency_bucket,
		       AVG(cache_hit_rate), AVG(success_rate), COUNT(*)
		FROM routing_patterns
		WHERE opted_in = TRUE
		  AND feature_category = $1
		  AND input_token_range = $2
		GROUP BY model_used, provider_used, input_token_range, latency_bucket
		ORDER BY AVG(output_quality) DESC, COUNT(*) DESC
		LIMIT 1`,
		feature, inputRange)
	var p RoutingPattern
	p.FeatureCategory = feature
	if err := row.Scan(&p.ModelUsed, &p.ProviderUsed, &p.InputTokenRange,
		&p.OutputQuality, &p.LatencyBucket, &p.CacheHitRate, &p.SuccessRate, &p.SampleCount); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pattern mining: top insight: %w", err)
	}
	return &p, nil
}

// ─── AggregateCohorts (routing intelligence, Upgrade 22) ───

// CohortStat is one aggregated (feature, input-range, model, provider)
// candidate across all OPTED-IN workspaces: its mean quality, how many
// patterns backed it, and — critically for the "don't override from a
// single workspace" rule — how many DISTINCT workspaces contributed.
type CohortStat struct {
	FeatureCategory    string  `json:"feature_category"`
	InputTokenRange    string  `json:"input_token_range"`
	ModelUsed          string  `json:"model_used"`
	ProviderUsed       string  `json:"provider_used"`
	AvgQuality         float64 `json:"avg_quality"`
	SampleCount        int     `json:"sample_count"`
	DistinctWorkspaces int     `json:"distinct_workspaces"`
}

const aggregateCohortsSQL = `
SELECT feature_category, input_token_range, model_used, provider_used,
       AVG(output_quality), COUNT(*), COUNT(DISTINCT workspace_id)
FROM routing_patterns
WHERE opted_in = TRUE
GROUP BY feature_category, input_token_range, model_used, provider_used`

// AggregateCohorts returns every candidate (feature, input-range, model,
// provider) over the opted-in corpus in ONE query — the routing advisor
// loads this into memory on a timer so the per-request path never hits the
// DB. Reads only the privacy-bucketed aggregate, never raw request content.
func (m *PatternMiner) AggregateCohorts(ctx context.Context) ([]CohortStat, error) {
	if m.pool == nil {
		return nil, nil
	}
	rows, err := m.pool.Query(ctx, aggregateCohortsSQL)
	if err != nil {
		return nil, fmt.Errorf("pattern mining: aggregate cohorts: %w", err)
	}
	defer rows.Close()
	var out []CohortStat
	for rows.Next() {
		var c CohortStat
		if err := rows.Scan(&c.FeatureCategory, &c.InputTokenRange, &c.ModelUsed,
			&c.ProviderUsed, &c.AvgQuality, &c.SampleCount, &c.DistinctWorkspaces); err != nil {
			return nil, fmt.Errorf("pattern mining: scan cohort: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ─── PatternRates ────────────────────────────────

// PatternRates exports the public rate table.
func PatternRates() map[string]float64 {
	return map[string]float64{
		"base_per_pattern":        PatternBaseRate,
		"rarity_multiplier_max":   RarityMultiplierMax,
		"unique_pattern_bonus":    UniquePatternBonus,
		"unique_rarity_threshold": UniqueRarityThreshold,
	}
}

// ─── RecordPatternObservation (Phase-3 capture write) ──────────────
//
// CAPTURE-ONLY. Persists ONE anonymized routing observation and does nothing
// else. It is structurally MINT-FREE: the body references no ledger and makes
// no Credit call (contrast RecordPattern, which credits when optedIn). Earning
// + anti-gaming is a SEPARATE later stage; this method cannot mint by
// construction, not by configuration.
//
// The write is gated on CONSENT in SQL: the conditional INSERT writes a row
// ONLY when the workspace has opted in (WHERE EXISTS over workspace_pattern_optin),
// so a non-opted-in workspace gets NO row. Persisted rows are always
// opted_in=TRUE / earned=0 (only the consented case is ever written) — making
// them visible to the already-live routing Advisor (which reads opted_in=TRUE)
// while crediting nothing.
const insertPatternObservationSQL = `
INSERT INTO routing_patterns (
	workspace_id, feature_category, model_used, provider_used,
	input_token_range, output_quality, latency_bucket,
	cache_hit_rate, success_rate, sample_count, rarity,
	opted_in, earned
)
SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 0, TRUE, 0
WHERE EXISTS (SELECT 1 FROM workspace_pattern_optin WHERE workspace_id = $1)`

// RecordPatternObservation persists a single anonymized routing observation for
// an OPTED-IN workspace (no-op for others). It never scores rarity, never
// computes earnings, and NEVER calls the ledger — capture is structurally
// incapable of minting. Best-effort: the caller invokes it post-serve on a
// detached context; errors are the caller's to log, not to propagate.
func (m *PatternMiner) RecordPatternObservation(ctx context.Context, workspaceID string, p RoutingPattern) error {
	if m == nil || m.pool == nil {
		return nil
	}
	if _, err := m.pool.Exec(ctx, insertPatternObservationSQL,
		workspaceID, p.FeatureCategory, p.ModelUsed, p.ProviderUsed,
		p.InputTokenRange, p.OutputQuality, p.LatencyBucket,
		p.CacheHitRate, p.SuccessRate, p.SampleCount,
	); err != nil {
		return fmt.Errorf("pattern mining: insert observation: %w", err)
	}
	return nil
}
