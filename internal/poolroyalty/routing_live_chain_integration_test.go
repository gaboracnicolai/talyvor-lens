package poolroyalty_test

// routing_live_chain_integration_test.go — the MINT-1 money-path proof (real PG): a LIVE-emitted routing
// prediction, once PROVEN skill-above-baseline by a score, is minted HELD by the REAL routing-prediction
// minter, EXAMINED by the single-party velocity detector, cleared, and SETTLED by the armed fail-closed
// finalize sweeper — while an UNPROVEN prediction (no score) mints nothing, so there is nothing to strand.
//
// It exercises the actual production symbols end to end:
//   routingpredict.Store.EmitLivePrediction  (the LIVE emit — writes status='active', NOT the seed CLI's
//                                              'pending'; this is what proxy.emitRoutingPrediction calls)
//   poolroyalty.NewRoutingPredictionMinter   (the REAL mint sweeper: rate × clamp01(skill_margin) → HELD)
//   poolroyalty.NewSinglePartyConcentrationDetector + NewSettlementClearer + NewFinalizeSweeper (examine→settle)
//
// The score row is inserted directly (the scorer, internal/routingscore, is independently tested and needs a
// live provider Inferer). Everything downstream of the score is the real chain.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/poolroyalty"
	"github.com/talyvor/lens/internal/routingpredict"
)

func routingChainPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping routing live-chain integration test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS routing_prediction_mints, routing_prediction_scores, routing_predictions,
			lens_token_balances, lens_token_ledger`,
		`CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY, balance BIGINT NOT NULL DEFAULT 0,
			held_balance BIGINT NOT NULL DEFAULT 0, lifetime_earned BIGINT NOT NULL DEFAULT 0,
			lifetime_spent BIGINT NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_ledger (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL,
			amount BIGINT NOT NULL, balance_after BIGINT NOT NULL, type TEXT NOT NULL,
			description TEXT, metadata JSONB, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		// routing_predictions (migration 0070) + the live-dedup partial unique the emit relies on.
		`CREATE TABLE routing_predictions (id TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
			workspace_id TEXT NOT NULL, feature_category TEXT NOT NULL, input_token_range TEXT NOT NULL,
			complexity_bucket TEXT NOT NULL DEFAULT '', model TEXT NOT NULL, provider TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'pending', created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE UNIQUE INDEX idx_routing_predictions_live ON routing_predictions
			(workspace_id, feature_category, input_token_range, complexity_bucket) WHERE status IN ('pending','active')`,
		// routing_prediction_scores (0072) — the scorer's output the minter joins on.
		`CREATE TABLE routing_prediction_scores (id TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
			prediction_id TEXT NOT NULL UNIQUE, slice_size INTEGER NOT NULL, m_avg DOUBLE PRECISION NOT NULL,
			baseline_avg DOUBLE PRECISION NOT NULL, baseline_model TEXT NOT NULL, skill_margin DOUBLE PRECISION NOT NULL,
			scored_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		// routing_prediction_mints (0073) — the generic single-party finalize columns.
		`CREATE TABLE routing_prediction_mints (request_id TEXT PRIMARY KEY, contributor_workspace_id TEXT NOT NULL,
			skill_margin DOUBLE PRECISION NOT NULL, minted_amount BIGINT NOT NULL, status TEXT NOT NULL DEFAULT 'held',
			finalize_after TIMESTAMPTZ NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool
}

// TestRoutingLiveChain_EmitScoredMintExaminedSettles is the MINT-1 acceptance proof.
func TestRoutingLiveChain_EmitScoredMintExaminedSettles(t *testing.T) {
	pool := routingChainPool(t)
	ctx := context.Background()
	ledger := mining.NewLedgerStore(pool) // nil verifier ⇒ U6 floor no-op (independently tested); chain focus here

	// ── (1) LIVE EMIT (the production path, NOT the seed CLI). EmitLivePrediction writes status='active'
	//        directly — a live routing decision is authoritative. This is exactly what proxy.emitRoutingPrediction
	//        calls when the cohort overrode the baseline.
	store := routingpredict.NewStore(pool, func() bool { return true })
	goodID, err := store.EmitLivePrediction(ctx, routingpredict.Prediction{
		WorkspaceID: "ws_good", FeatureCategory: "chat", InputTokenRange: "small",
		ComplexityBucket: "simple", Model: "gpt-4o", Provider: "openai",
	})
	if err != nil {
		t.Fatalf("live emit (good): %v", err)
	}
	// An UNPROVEN prediction — emitted live, but the scorer never produces a score for it (warm-up floor /
	// baseline==M / skill≤0). It must mint NOTHING (the farm-zero floor at the mint layer).
	unprovenID, err := store.EmitLivePrediction(ctx, routingpredict.Prediction{
		WorkspaceID: "ws_farm", FeatureCategory: "code", InputTokenRange: "small",
		ComplexityBucket: "trivial", Model: "gpt-4o-mini", Provider: "openai",
	})
	if err != nil {
		t.Fatalf("live emit (unproven): %v", err)
	}

	// PROOF the LIVE path fired (not the seed CLI): the emitted rows are 'active', not 'pending'.
	var goodStatus string
	_ = pool.QueryRow(ctx, `SELECT status FROM routing_predictions WHERE id=$1`, goodID).Scan(&goodStatus)
	if goodStatus != "active" {
		t.Fatalf("live-emitted prediction status=%q, want 'active' (the LIVE path writes active directly; the seed CLI writes 'pending')", goodStatus)
	}

	// ── (2) SCORE the GOOD prediction skill-above-baseline (the scorer's output; independently tested).
	//        skill_margin 0.5 = the predicted model beat the baseline by 0.5 on the held eval slice.
	const margin = 0.5
	if _, err := pool.Exec(ctx, `INSERT INTO routing_prediction_scores
		(prediction_id, slice_size, m_avg, baseline_avg, baseline_model, skill_margin)
		VALUES ($1, 5, 0.9, 0.4, 'gpt-4o-mini', $2)`, goodID, margin); err != nil {
		t.Fatalf("seed score: %v", err)
	}
	// NO score for unprovenID — it never cleared the scorer.

	// ── (3) MINT via the REAL minter: amount = rate × clamp01(skill_margin). Rate 0.02 = the shipped
	//        coherence rate (config.go). A tiny holdback so finalize_after is immediately past.
	const rate = 0.02
	minter := poolroyalty.NewRoutingPredictionMinter(pool, ledger, rate, func() bool { return true })
	minter.SetHoldbackWindow(time.Millisecond)
	minted, err := minter.RunOnce(ctx)
	if err != nil {
		t.Fatalf("minter.RunOnce: %v", err)
	}
	if minted != 1 {
		t.Fatalf("minted %d predictions, want 1 (only the SCORED, skill-positive one; the unproven mints nothing)", minted)
	}
	wantAmount := mining.FloatToMicroFloor(rate * margin) // floor(0.02 × 0.5 × 1e6) = 10000 µLENS
	var mintedAmount int64
	var mintStatus string
	if err := pool.QueryRow(ctx, `SELECT minted_amount, status FROM routing_prediction_mints WHERE request_id=$1`, goodID).
		Scan(&mintedAmount, &mintStatus); err != nil {
		t.Fatalf("read mint row: %v", err)
	}
	if mintedAmount != wantAmount {
		t.Errorf("minted_amount=%d, want %d (rate 0.02 × margin 0.5 → µLENS floor)", mintedAmount, wantAmount)
	}
	if mintStatus != "held" {
		t.Errorf("fresh mint status=%q, want 'held' (routed through the held kernel, not direct-to-spendable)", mintStatus)
	}
	// The unproven prediction produced NO mint row — nothing to strand.
	var unprovenMints int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_prediction_mints WHERE request_id=$1`, unprovenID).Scan(&unprovenMints)
	if unprovenMints != 0 {
		t.Errorf("unproven prediction produced %d mint rows, want 0 (farm-zero: an unscored prediction never mints)", unprovenMints)
	}

	// ── (4) EXAMINE (single-party velocity detector; velocityMax high ⇒ the honest single mint is examined,
	//        unflagged) → CLEAR → SETTLE via the armed fail-closed FinalizeSweeper. This is the SAME gate the
	//        examination test proves withholds a farm; here it lets an honest, examined mint settle.
	det := poolroyalty.NewSinglePartyConcentrationDetector(pool, "routing_prediction_mints", 100, 24*time.Hour)
	clearer := poolroyalty.NewSettlementClearer(det, pool, "routing_prediction_mints", func() bool { return true }, 24*time.Hour)
	if _, err := clearer.RunOnce(ctx); err != nil {
		t.Fatalf("clearer.RunOnce: %v", err)
	}
	time.Sleep(4 * time.Millisecond) // let finalize_after pass
	sw := poolroyalty.NewFinalizeSweeper(pool, ledger, "routing_prediction_mints")
	sw.SetSettleStatus("cleared") // armed fail-closed: settle ONLY examined→cleared rows
	if _, err := sw.RunOnce(ctx); err != nil {
		t.Fatalf("finalize.RunOnce: %v", err)
	}

	// ── (5) ASSERT: the GOOD author's held value SETTLED to spendable at the coherent rate; nothing stranded.
	var goodBal, goodHeld int64
	_ = pool.QueryRow(ctx, `SELECT COALESCE(balance,0), COALESCE(held_balance,0) FROM lens_token_balances WHERE workspace_id='ws_good'`).
		Scan(&goodBal, &goodHeld)
	if goodBal != wantAmount || goodHeld != 0 {
		t.Errorf("good author spendable=%d held=%d, want %d/0 (examined→cleared→settled, no strand)", goodBal, goodHeld, wantAmount)
	}
	// The unproven author never earned anything.
	var farmBal, farmHeld int64
	_ = pool.QueryRow(ctx, `SELECT COALESCE(balance,0), COALESCE(held_balance,0) FROM lens_token_balances WHERE workspace_id='ws_farm'`).
		Scan(&farmBal, &farmHeld)
	if farmBal != 0 || farmHeld != 0 {
		t.Errorf("unproven author spendable=%d held=%d, want 0/0 (never scored ⇒ never minted)", farmBal, farmHeld)
	}
	// No held row lingers for the settled mint (it did not strand).
	var lingering int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_prediction_mints WHERE status='held'`).Scan(&lingering)
	if lingering != 0 {
		t.Errorf("%d held mint rows linger post-settle, want 0 (examined+cleared+settled — nothing stranded)", lingering)
	}
}
