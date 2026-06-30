package poolroyalty

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/mining"
)

// Real-PG proofs for proof-of-routing-prediction (Proof-of-Improvement instance 2):
//   - INERT: rate 0 + flag ON mints nothing; a positive rate mints rate×clamp01(skill_margin) once.
//   - DEDUP: a second sweep mints nothing (once-per-scored-prediction via request_id = prediction_id).
//   - U6: an unverified author mints nothing (ErrEarnNotVerified rolls back); the credit is a
//     TypeRoutingPredictionHeld held mint (∈ mintTypeList ⇒ the same U6 floor + 24h rate cap).
//   - ATTRIBUTION: the credit lands on routing_predictions.workspace_id (the prediction's author).
//   - ADVERSARIAL: skill_margin ≤ 0 mints nothing; a prediction with NO score row mints nothing (the
//     minter pays only on scores the scorer actually wrote — the #254 author-exclusion is upstream of the
//     score, so a self-dealt score never exists for the minter to see; see routingscore
//     TestScore_AuthorExclusion_Integration).

func routingHarness(t *testing.T) (*pgxpool.Pool, *mining.LedgerStore) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG routing-prediction mint test")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "lens_routingpredmint_test"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP SCHEMA IF EXISTS lens_routingpredmint_test CASCADE`,
		`CREATE SCHEMA lens_routingpredmint_test`,
		`CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY, balance DOUBLE PRECISION NOT NULL DEFAULT 0,
			held_balance DOUBLE PRECISION NOT NULL DEFAULT 0, lifetime_earned DOUBLE PRECISION NOT NULL DEFAULT 0,
			lifetime_spent DOUBLE PRECISION NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_ledger (id UUID NOT NULL DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL,
			amount DOUBLE PRECISION NOT NULL, balance_after DOUBLE PRECISION NOT NULL, type TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '', metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (id, workspace_id))`,
		// The claim table — generic finalize columns (request_id = prediction_id, contributor_workspace_id = payee).
		`CREATE TABLE routing_prediction_mints (request_id TEXT PRIMARY KEY, contributor_workspace_id TEXT NOT NULL,
			skill_margin DOUBLE PRECISION NOT NULL, minted_amount DOUBLE PRECISION NOT NULL,
			status TEXT NOT NULL DEFAULT 'held', finalize_after TIMESTAMPTZ NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		// The scorer's output the minter reads (subset of 0072).
		`CREATE TABLE routing_prediction_scores (prediction_id TEXT PRIMARY KEY, slice_size INTEGER NOT NULL DEFAULT 3,
			m_avg DOUBLE PRECISION NOT NULL DEFAULT 0, baseline_avg DOUBLE PRECISION NOT NULL DEFAULT 0,
			baseline_model TEXT NOT NULL DEFAULT '', skill_margin DOUBLE PRECISION NOT NULL, scored_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		// The prediction (the payee chain) — subset of 0070.
		`CREATE TABLE routing_predictions (id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, feature_category TEXT NOT NULL DEFAULT '',
			input_token_range TEXT NOT NULL DEFAULT '', complexity_bucket TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '',
			provider TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'active', created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE workspaces (id TEXT PRIMARY KEY, earn_verified BOOLEAN NOT NULL DEFAULT false)`,
		`CREATE TABLE lxc_purchases (workspace_id TEXT NOT NULL, status TEXT NOT NULL, lxc_amount DOUBLE PRECISION NOT NULL)`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	ledger := mining.NewLedgerStore(pool)
	ledger.SetMintVerifier(earnverify.New()) // U6 floor — wired exactly as production
	return pool, ledger
}

// routingSeedScored inserts a prediction + its score (skill_margin) — the state the minter sweeps.
func routingSeedScored(t *testing.T, pool *pgxpool.Pool, predID, workspaceID string, skillMargin float64) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO routing_predictions (id, workspace_id) VALUES ($1,$2)`, predID, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO routing_prediction_scores (prediction_id, skill_margin) VALUES ($1,$2)`, predID, skillMargin); err != nil {
		t.Fatal(err)
	}
}

func routingMintRows(t *testing.T, pool *pgxpool.Pool, ws string) (n int, amount, margin float64) {
	t.Helper()
	_ = pool.QueryRow(context.Background(),
		`SELECT count(*), COALESCE(sum(minted_amount),0), COALESCE(max(skill_margin),0)
		 FROM routing_prediction_mints WHERE contributor_workspace_id=$1`, ws).Scan(&n, &amount, &margin)
	return
}

// (proof 1 + 2 + 3 + 5) INERT then LIVE, dedup, amount math, attribution.
func TestRoutingPrediction_InertThenLive_Integration(t *testing.T) {
	pool, ledger := routingHarness(t)
	ctx := context.Background()
	verifyWorkspace(t, pool, "wsAuthor")
	routingSeedScored(t, pool, "pred1", "wsAuthor", 0.5) // skill_margin 0.5

	bothOn := func() bool { return true }

	// INERT: rate 0 → anchor nil → no mint despite an eligible scored prediction + flags on.
	inert := NewRoutingPredictionMinter(pool, ledger, 0, bothOn)
	if n, err := inert.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("rate-0 minter must mint 0 with both flags on: n=%d err=%v", n, err)
	}
	if n, _, _ := routingMintRows(t, pool, "wsAuthor"); n != 0 {
		t.Fatalf("INERT: no routing_prediction_mints row must exist, got %d", n)
	}

	// LIVE: rate 10 → mints once at 10 × 0.5 = 5.0 (amount math).
	live := NewRoutingPredictionMinter(pool, ledger, 10, bothOn)
	n, err := live.RunOnce(ctx)
	if err != nil || n != 1 {
		t.Fatalf("rate-10 minter must mint exactly 1: n=%d err=%v", n, err)
	}
	rows, amount, margin := routingMintRows(t, pool, "wsAuthor")
	if rows != 1 || margin != 0.5 || amount < 4.99 || amount > 5.01 {
		t.Fatalf("expected one mint of 5.0 (margin 0.5 × rate 10): rows=%d amount=%.3f margin=%.3f", rows, amount, margin)
	}
	// ATTRIBUTION: the credit lands on the prediction's workspace (wsAuthor), nobody else.
	if _, held := balances(t, pool, "wsAuthor"); held < 4.99 || held > 5.01 {
		t.Fatalf("author held_balance %.3f, want 5.0", held)
	}
	// DEDUP: a second sweep mints nothing more (once-per-scored-prediction claim).
	if n2, _ := live.RunOnce(ctx); n2 != 0 {
		t.Fatalf("second sweep must mint 0 (once-per-prediction), got %d", n2)
	}
}

// (proof 4) U6: an UNVERIFIED author mints nothing (the held-ledger verifyEarn floor rolls it back);
// verifying then lets the SAME prediction mint. The credit type is TypeRoutingPredictionHeld (∈ mintTypeList
// ⇒ the same U6 floor + 24h rate cap; NOT reputation-bonded — decision c).
func TestRoutingPrediction_U6Floor_Integration(t *testing.T) {
	pool, ledger := routingHarness(t)
	ctx := context.Background()
	routingSeedScored(t, pool, "predU", "wsUnverified", 0.5) // wsUnverified is NOT earn_verified
	live := NewRoutingPredictionMinter(pool, ledger, 10, func() bool { return true })

	if n, err := live.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("unverified author must mint 0 (U6 floor rolls back): n=%d err=%v", n, err)
	}
	if n, _, _ := routingMintRows(t, pool, "wsUnverified"); n != 0 {
		t.Fatalf("U6: claim row must be rolled back, got %d", n)
	}
	// Verify → the SAME prediction now mints, isolating the U6 floor as the only blocker.
	verifyWorkspace(t, pool, "wsUnverified")
	if n, err := live.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("verified author must now mint 1: n=%d err=%v", n, err)
	}
	var typ string
	if err := pool.QueryRow(ctx, `SELECT type FROM lens_token_ledger WHERE workspace_id='wsUnverified' ORDER BY created_at DESC LIMIT 1`).Scan(&typ); err != nil {
		t.Fatal(err)
	}
	if typ != mining.TypeRoutingPredictionHeld {
		t.Fatalf("routing mint ledger type = %q, want %q (∈ mintTypeList ⇒ U6 floor + 24h cap)", typ, mining.TypeRoutingPredictionHeld)
	}
}

// (proof 8) ADVERSARIAL: skill_margin ≤ 0 mints nothing (M did not beat the baseline); a prediction with
// NO score row mints nothing (the minter pays ONLY on scores the scorer actually wrote — and the scorer
// excludes the author from the slice UPSTREAM, so a self-dealt score never reaches this table).
func TestRoutingPrediction_NonPositiveAndUnscored_Integration(t *testing.T) {
	pool, ledger := routingHarness(t)
	ctx := context.Background()
	verifyWorkspace(t, pool, "wsZero")
	verifyWorkspace(t, pool, "wsNeg")
	verifyWorkspace(t, pool, "wsNoScore")
	live := NewRoutingPredictionMinter(pool, ledger, 10, func() bool { return true })

	// (a) skill_margin exactly 0 (M tied the baseline) → no mint.
	routingSeedScored(t, pool, "predZero", "wsZero", 0.0)
	// (b) skill_margin negative (shouldn't occur post-clamp, but defend anyway) → no mint.
	routingSeedScored(t, pool, "predNeg", "wsNeg", -0.3)
	// (c) a prediction with NO score row — the scorer never scored it (e.g. author-excluded slice / BasisNone).
	if _, err := pool.Exec(ctx, `INSERT INTO routing_predictions (id, workspace_id) VALUES ('predNoScore','wsNoScore')`); err != nil {
		t.Fatal(err)
	}

	n, err := live.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 0 {
		t.Fatalf("non-positive margins + an unscored prediction must mint 0, got %d", n)
	}
	for _, ws := range []string{"wsZero", "wsNeg", "wsNoScore"} {
		if got, _, _ := routingMintRows(t, pool, ws); got != 0 {
			t.Fatalf("%s must have no mint row, got %d", ws, got)
		}
	}
}
