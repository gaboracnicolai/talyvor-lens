package proxy

// OVERRIDE-RATE HARNESS (integration, test-only — NOT production). Drives SYNTHETIC traffic through the REAL
// auto-route path (routing.Advisor.Recommend → router.Route → resolveAutoRoute) against a real PG, records via
// the same fields captureRouteDecision writes, and reads the result through the REAL routedecision.Summarize.
// It measures how often cohort intelligence overrides the baseline, and — the honest part — whether those
// overrides SAVE or SPEND. This is SYNTHETIC traffic on a SEEDED corpus: it shows what the MECHANISM does under
// a plausible corpus, NOT what real customers will do. It is a go/no-go signal, not a calibrated number.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/routedecision"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/routing"
	"github.com/talyvor/lens/internal/workspace"
	"github.com/talyvor/lens/internal/worktier"
)

func harnessPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping override-rate harness")
	}
	// Private schema (the Lens gated-test convention): the Advisor + routedecision writer read/write the
	// hardcoded table names routing_patterns / routing_decisions, so this harness cannot use unique names — it
	// isolates via its OWN search_path instead, so its DROP/CREATE never clobbers other proxy tests'
	// routing_patterns (e.g. pattern_earn_test.go) on the shared DB.
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "lens_it_overrideharness"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), `
		DROP SCHEMA IF EXISTS lens_it_overrideharness CASCADE; CREATE SCHEMA lens_it_overrideharness;`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `
		CREATE TABLE routing_patterns (
			id BIGSERIAL PRIMARY KEY, workspace_id TEXT NOT NULL, feature_category TEXT NOT NULL,
			model_used TEXT NOT NULL, provider_used TEXT NOT NULL, input_token_range TEXT NOT NULL,
			output_quality DOUBLE PRECISION NOT NULL DEFAULT 0, latency_bucket TEXT NOT NULL DEFAULT 'fast',
			cache_hit_rate DOUBLE PRECISION NOT NULL DEFAULT 0, success_rate DOUBLE PRECISION NOT NULL DEFAULT 1,
			sample_count INTEGER NOT NULL DEFAULT 1, rarity DOUBLE PRECISION NOT NULL DEFAULT 0,
			complexity_bucket TEXT NOT NULL DEFAULT '', opted_in BOOLEAN NOT NULL DEFAULT FALSE,
			earned BIGINT NOT NULL DEFAULT 0, created_at TIMESTAMPTZ NOT NULL DEFAULT now());
		CREATE TABLE routing_decisions (
			id BIGSERIAL PRIMARY KEY, workspace_id TEXT NOT NULL, baseline_model TEXT NOT NULL, actual_model TEXT NOT NULL,
			cohort_overrode BOOLEAN NOT NULL, cohort_basis TEXT NOT NULL DEFAULT '', cohort_n INTEGER NOT NULL DEFAULT 0,
			input_tokens INTEGER NOT NULL, output_tokens INTEGER NOT NULL, actual_cost_u BIGINT NOT NULL,
			counterfactual_cost_estimate_u BIGINT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return pool
}

// seedCohort inserts `nWorkspaces` × `perWs` opted-in rows for one (feature,range,model,provider) at `quality`.
// AggregateCohorts qualifies a cohort at COUNT(*) ≥ 20 AND COUNT(DISTINCT workspace_id) ≥ 3.
func seedCohort(t *testing.T, pool *pgxpool.Pool, feature, inputRange, model, provider string, quality float64, nWorkspaces, perWs int) {
	t.Helper()
	ctx := context.Background()
	for w := 0; w < nWorkspaces; w++ {
		ws := fmt.Sprintf("cohortWs_%s_%s_%d", feature, model, w)
		for s := 0; s < perWs; s++ {
			if _, err := pool.Exec(ctx, `INSERT INTO routing_patterns
				(workspace_id, feature_category, model_used, provider_used, input_token_range, output_quality, opted_in)
				VALUES ($1,$2,$3,$4,$5,$6,TRUE)`, ws, feature, model, provider, inputRange, quality); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
	}
}

type scenario struct {
	feature string
	prompt  string
}

// runMeasurement drives `reps` passes of the scenario set through the real path and returns the summary.
func runMeasurement(t *testing.T, pool *pgxpool.Pool, adv *routing.Advisor, rt *router.Router, scenarios []scenario, reps int) routedecision.Summary {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `TRUNCATE routing_decisions`); err != nil {
		t.Fatal(err)
	}
	if err := adv.Refresh(ctx); err != nil { // the ONLY DB read — snapshot the seeded corpus
		t.Fatalf("refresh: %v", err)
	}
	writer := routedecision.NewWriter(pool)
	const outTok = 300
	for r := 0; r < reps; r++ {
		for _, sc := range scenarios {
			compressed := sc.prompt
			inTok := len(compressed) / 4
			cScore := router.AnalyseComplexity(compressed).Score()
			rec := adv.Recommend(ctx, "wsTest", sc.feature, inTok,
				string(worktier.ComplexityBucketFor(cScore)), "openai", nil, nil)
			base := rt.Route(ctx, "openai", "auto", compressed)
			dt := newDecisionTier(inTok, cScore, false, false, workspace.LoggingMetadata)
			res := resolveAutoRoute(rt, rec, base, dt)
			served := res.model
			if served == "" {
				served = base.Model
			}
			if err := writer.Record(ctx, routedecision.RouteDecision{
				WorkspaceID: "wsTest", BaselineModel: base.Model, ActualModel: served,
				CohortOverrode: res.applied, CohortBasis: string(rec.Basis), CohortN: rec.DistinctWorkspaces,
				InputTokens: inTok, OutputTokens: outTok,
				ActualCostU:                 usdToMicroUSD(alerts.CostUSD(served, inTok, outTok)),
				CounterfactualCostEstimateU: usdToMicroUSD(alerts.CostUSD(base.Model, inTok, outTok)),
			}); err != nil {
				t.Fatalf("record: %v", err)
			}
		}
	}
	s, err := routedecision.NewReader(pool).Summarize(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	return s
}

// realizedChangeRate counts rows where the SERVED model actually differs from the baseline (a real route
// change, not a no-op "override" that recommends the same model the baseline already picks).
func realizedChangeRate(t *testing.T, pool *pgxpool.Pool) (changed, total int) {
	t.Helper()
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FILTER (WHERE actual_model <> baseline_model), COUNT(*) FROM routing_decisions`).Scan(&changed, &total)
	return
}

func TestOverrideRate_Measurement(t *testing.T) {
	pool := harnessPool(t)
	// A plausible corpus: 4 features, each seeded with the 3 openai models a real workspace would compare, at a
	// realistic quality spread. Real catalog costs come from alerts.CostUSD (the Advisor's cost function).
	costFn := func(m string) float64 { return alerts.CostUSD(m, 500, 500) }
	adv := routing.New(mining.NewPatternMiner(nil, pool), costFn, routing.Config{Enabled: true})
	rt := router.New()

	features := []string{"chat", "code", "summarize", "extract"}
	// quality by model — a plausible spread (mini decent, 4.1/4o better). small input range (short prompts).
	seedFull := func(nWs int) {
		if _, err := pool.Exec(context.Background(), `TRUNCATE routing_patterns`); err != nil {
			t.Fatal(err)
		}
		for _, f := range features {
			seedCohort(t, pool, f, "small", "gpt-4o-mini", "openai", 0.74, nWs, 7)
			seedCohort(t, pool, f, "small", "gpt-4.1", "openai", 0.88, nWs, 7)
			seedCohort(t, pool, f, "small", "gpt-4o", "openai", 0.90, nWs, 7)
		}
	}
	// short prompts → low complexity → base router picks the cheapest (gpt-4o-mini) for openai.
	scenarios := []scenario{
		{"chat", "What is the capital of France?"},
		{"code", "Write a one-line bash command to list files."},
		{"summarize", "Summarize: the meeting is at noon."},
		{"extract", "Extract the date from: due on Friday."},
	}

	// ── primary measurement (dense corpus) ──
	seedFull(10)
	s := runMeasurement(t, pool, adv, rt, scenarios, 6)
	changed, total := realizedChangeRate(t, pool)
	t.Logf("PRIMARY (dense corpus, 10 ws/cohort, %d requests):", total)
	t.Logf("  override_rate (cohort applied)      = %.1f%% (%d/%d)", s.OverrideRate*100, s.OverrideCount, s.TotalRequests)
	t.Logf("  realized_change_rate (served≠base)  = %.1f%% (%d/%d)", 100*float64(changed)/float64(max1(total)), changed, total)
	t.Logf("  net estimated cost delta (µ-USD)    = %d  (counterfactual−actual; >0 saves, <0 spends)", s.EstimatedCostDeltaU)
	t.Logf("  aggregate actual=%d counterfactual=%d (µ-USD)", s.TotalActualCostU, s.TotalCounterfactualEstimateU)

	// ── density curve: override rate vs distinct workspaces per cohort (the ≥3 floor is the moat gate) ──
	t.Logf("DENSITY CURVE (override rate vs workspaces/cohort; floor = 3):")
	for _, nWs := range []int{1, 2, 3, 5, 10} {
		seedFull(nWs)
		sc := runMeasurement(t, pool, adv, rt, scenarios, 6)
		ch, tot := realizedChangeRate(t, pool)
		t.Logf("  ws/cohort=%2d → override_rate=%.1f%%  realized_change=%.1f%%  net_delta_u=%d",
			nWs, sc.OverrideRate*100, 100*float64(ch)/float64(max1(tot)), sc.EstimatedCostDeltaU)
	}

	// ── coverage curve: override rate vs fraction of request features that have a qualifying cohort ──
	t.Logf("COVERAGE CURVE (override rate vs #features with a qualifying cohort, of %d):", len(features))
	for _, covered := range []int{0, 1, 2, 4} {
		if _, err := pool.Exec(context.Background(), `TRUNCATE routing_patterns`); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < covered; i++ {
			f := features[i]
			seedCohort(t, pool, f, "small", "gpt-4o-mini", "openai", 0.74, 10, 7)
			seedCohort(t, pool, f, "small", "gpt-4.1", "openai", 0.88, 10, 7)
			seedCohort(t, pool, f, "small", "gpt-4o", "openai", 0.90, 10, 7)
		}
		sc := runMeasurement(t, pool, adv, rt, scenarios, 6)
		t.Logf("  features_covered=%d/%d → override_rate=%.1f%%  net_delta_u=%d",
			covered, len(features), sc.OverrideRate*100, sc.EstimatedCostDeltaU)
	}

	// The harness must not silently pass while measuring nothing — assert it actually drove traffic.
	if s.TotalRequests == 0 {
		t.Fatal("harness recorded zero requests — the measurement did not run")
	}
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// TestOverrideRate_SavingsPathsBlocked exercises the only two paths where an override changes the served model,
// to show WHY the realized cost delta is ~0: (1) a cohort DOWNGRADE on a non-small-simple request is VETOED
// (no saving realized), and (2) a cohort UPGRADE is applied but SPENDS (negative delta). There is no path that
// realizes a saving, because the base router already picks the cheapest model for cheap requests.
func TestOverrideRate_SavingsPathsBlocked(t *testing.T) {
	pool := harnessPool(t)
	ctx := context.Background()
	costFn := func(m string) float64 { return alerts.CostUSD(m, 500, 500) }
	adv := routing.New(mining.NewPatternMiner(nil, pool), costFn, routing.Config{Enabled: true})
	rt := router.New()

	// (2) UPGRADE-SPEND: a cohort with mini ABSENT → qpd winner is gpt-4.1. A simple request's baseline is
	// gpt-4o-mini, so applying the rec is an UPGRADE (not a downgrade → not vetoed) that SPENDS.
	seedCohort(t, pool, "up", "small", "gpt-4.1", "openai", 0.88, 10, 7)
	seedCohort(t, pool, "up", "small", "gpt-4o", "openai", 0.90, 10, 7)
	// (1) DOWNGRADE-VETO: a cohort where mini wins qpd. A COMPLEX request's baseline is a pricier model, so the
	// rec (mini) is a strict downgrade → VETOED on a non-small-simple request → baseline served, no saving.
	seedCohort(t, pool, "down", "large", "gpt-4o-mini", "openai", 0.80, 10, 7)
	seedCohort(t, pool, "down", "large", "gpt-4.1", "openai", 0.86, 10, 7)
	if err := adv.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	drive := func(feature, prompt string) (baseM, served string, applied bool, deltaU int64) {
		inTok := len(prompt) / 4
		cScore := router.AnalyseComplexity(prompt).Score()
		rec := adv.Recommend(ctx, "wsTest", feature, inTok, string(worktier.ComplexityBucketFor(cScore)), "openai", nil, nil)
		base := rt.Route(ctx, "openai", "auto", prompt)
		dt := newDecisionTier(inTok, cScore, false, false, workspace.LoggingMetadata)
		res := resolveAutoRoute(rt, rec, base, dt)
		served = res.model
		if served == "" {
			served = base.Model
		}
		deltaU = usdToMicroUSD(alerts.CostUSD(base.Model, inTok, 300)) - usdToMicroUSD(alerts.CostUSD(served, inTok, 300))
		return base.Model, served, res.applied, deltaU
	}

	// (2) simple request → base gpt-4o-mini; cohort (mini-absent) recommends gpt-4.1 → UPGRADE applied → SPEND.
	baseM, served, applied, delta := drive("up", "What is the capital of France?")
	t.Logf("UPGRADE path: base=%s served=%s applied=%v delta_u=%d", baseM, served, applied, delta)
	if !applied || served == baseM {
		t.Errorf("upgrade must apply and change the model (base=%s served=%s applied=%v)", baseM, served, applied)
	}
	if delta >= 0 {
		t.Errorf("upgrade must SPEND (delta<0), got delta_u=%d — an upgrade is not a saving", delta)
	}

	// (1) complex request → base pricier; cohort recommends mini (a downgrade) → VETOED → baseline served.
	complexPrompt := "Write a Python function to compute the factorial recursively, then prove its correctness " +
		"by induction and analyze the time and space complexity step by step with a worked example."
	baseM2, served2, _, delta2 := drive("down", complexPrompt)
	t.Logf("DOWNGRADE path: base=%s served=%s delta_u=%d (want served==base: veto blocks the cohort downgrade)", baseM2, served2, delta2)
	if served2 != baseM2 {
		t.Errorf("a cohort downgrade on a non-small-simple request must be VETOED (served==base); base=%s served=%s", baseM2, served2)
	}
	if served2 == "gpt-4o-mini" {
		t.Errorf("the veto failed — a complex request was downgraded to mini, realizing a quality risk the guard exists to prevent")
	}
}
