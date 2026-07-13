package poolroyalty

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/mining"
)

// Real-PG proofs for proof-of-eval-contribution (Proof-of-Improvement instance 1):
//   - INERT: rate 0 + flag ON mints nothing; a positive rate then mints rate×discrimination once.
//   - WARMUP: < MinUnlinkedGraders distinct graders pays zero; ≥ N with a split pays rate×4·Var.
//   - U6: an unverified author mints nothing (ErrEarnNotVerified rolls back); the credit is a
//     TypeEvalContributionHeld held mint (∈ mintTypeList ⇒ the same U6 floor + 24h rate cap).

func evalHarness(t *testing.T) (*pgxpool.Pool, *mining.LedgerStore) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG eval-contribution test")
	}
	// Private schema (not public): the CI test DB has the FULL migrated schema where node_metrics has an
	// FK to inference_nodes, so DROP-ing migration-owned tables in public fails. A dedicated schema fully
	// isolates this harness's tables from the migrated public ones (and avoids cross-package -p collisions).
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "lens_evalcontrib_test"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP SCHEMA IF EXISTS lens_evalcontrib_test CASCADE`,
		`CREATE SCHEMA lens_evalcontrib_test`,
		`CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY, balance BIGINT NOT NULL DEFAULT 0,
			held_balance BIGINT NOT NULL DEFAULT 0, lifetime_earned BIGINT NOT NULL DEFAULT 0,
			lifetime_spent BIGINT NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_ledger (id UUID NOT NULL DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL,
			amount BIGINT NOT NULL, balance_after BIGINT NOT NULL, type TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '', metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (id, workspace_id))`,
		`CREATE TABLE eval_contribution_mints (request_id TEXT PRIMARY KEY, contributor_workspace_id TEXT NOT NULL,
			discrimination DOUBLE PRECISION NOT NULL, distinct_graders INTEGER NOT NULL, minted_amount BIGINT NOT NULL,
			status TEXT NOT NULL DEFAULT 'held', finalize_after TIMESTAMPTZ NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE benchmark_eval_items (id TEXT PRIMARY KEY, input TEXT NOT NULL, expected_output TEXT NOT NULL,
			eval_method TEXT NOT NULL DEFAULT 'exact', pass_threshold DOUBLE PRECISION NOT NULL DEFAULT 1.0,
			active BOOLEAN NOT NULL DEFAULT TRUE, content_hash TEXT, status TEXT NOT NULL DEFAULT 'active',
			author_workspace_id TEXT, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE benchmark_probes (id TEXT PRIMARY KEY, node_id TEXT NOT NULL, item_id TEXT NOT NULL,
			request_id TEXT, served_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), score DOUBLE PRECISION NOT NULL DEFAULT 0,
			UNIQUE (node_id, item_id))`,
		`CREATE TABLE inference_nodes (id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, url TEXT NOT NULL DEFAULT '',
			provider TEXT NOT NULL DEFAULT 'vllm')`,
		`CREATE TABLE workspace_card_fingerprints (workspace_id TEXT NOT NULL, fingerprint_hash TEXT NOT NULL,
			PRIMARY KEY (workspace_id, fingerprint_hash))`,
		`CREATE TABLE workspaces (id TEXT PRIMARY KEY, earn_verified BOOLEAN NOT NULL DEFAULT false)`,
		`CREATE TABLE lxc_purchases (workspace_id TEXT NOT NULL, status TEXT NOT NULL, lxc_amount BIGINT NOT NULL)`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	ledger := mining.NewLedgerStore(pool)
	ledger.SetMintVerifier(earnverify.New()) // U6 floor — wired exactly as production
	return pool, ledger
}

// seedItem inserts a validated, authored item; graders adds one node+probe per (workspace, score).
func evalSeedItem(t *testing.T, pool *pgxpool.Pool, itemID, author string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO benchmark_eval_items (id, input, expected_output, status, author_workspace_id)
		 VALUES ($1,'in','out','active',$2)`, itemID, author); err != nil {
		t.Fatal(err)
	}
}

func evalGrade(t *testing.T, pool *pgxpool.Pool, itemID, graderWS string, score float64) {
	t.Helper()
	ctx := context.Background()
	node := "node-" + graderWS
	if _, err := pool.Exec(ctx, `INSERT INTO inference_nodes (id, workspace_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, node, graderWS); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO benchmark_probes (id, node_id, item_id, score) VALUES ($1,$2,$3,$4)`,
		fmt.Sprintf("p-%s-%s", itemID, graderWS), node, itemID, score); err != nil {
		t.Fatal(err)
	}
}

func evalMintRows(t *testing.T, pool *pgxpool.Pool, author string) (n int, amount int64, discrimination float64) {
	t.Helper()
	_ = pool.QueryRow(context.Background(),
		`SELECT count(*), COALESCE(sum(minted_amount),0), COALESCE(max(discrimination),0)
		 FROM eval_contribution_mints WHERE contributor_workspace_id=$1`, author).Scan(&n, &amount, &discrimination)
	return
}

// (proof 4) INERT then LIVE: rate 0 + flag on mints nothing; a positive rate mints rate×discrimination once.
func TestEvalContribution_InertThenLive_Integration(t *testing.T) {
	pool, ledger := evalHarness(t)
	ctx := context.Background()
	verifyWorkspace(t, pool, "authorA")
	evalSeedItem(t, pool, "item1", "authorA")
	// 3 distinct unlinked graders, scores 1,1,0 → mean .667, Var_pop .2222, 4·Var ≈ .8889.
	evalGrade(t, pool, "item1", "wsG1", 1.0)
	evalGrade(t, pool, "item1", "wsG2", 1.0)
	evalGrade(t, pool, "item1", "wsG3", 0.0)

	bothOn := func() bool { return true } // both gating flags on

	// INERT: rate 0 → anchor nil → no mint despite an eligible item + flags on.
	inert := NewEvalContributionMinter(pool, ledger, 0, bothOn)
	if n, err := inert.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("rate-0 minter must mint 0 with both flags on: n=%d err=%v", n, err)
	}
	if n, _, _ := evalMintRows(t, pool, "authorA"); n != 0 {
		t.Fatalf("INERT: no eval_contribution_mints row must exist, got %d", n)
	}

	// LIVE: rate 10 → mints once at 10 × 0.8889 ≈ 8.89.
	live := NewEvalContributionMinter(pool, ledger, 10, bothOn)
	n, err := live.RunOnce(ctx)
	if err != nil || n != 1 {
		t.Fatalf("rate-10 minter must mint exactly 1: n=%d err=%v", n, err)
	}
	rows, amount, disc := evalMintRows(t, pool, "authorA")
	if rows != 1 || disc < 0.85 || disc > 0.92 || amount < micro(8.5) || amount > micro(9.2) {
		t.Fatalf("expected one mint ~8.89 (disc ~0.889): rows=%d amount=%d disc=%.3f", rows, amount, disc)
	}
	if _, held := balances(t, pool, "authorA"); held < micro(8.5) || held > micro(9.2) {
		t.Fatalf("author held_balance %d µLENS, want ~8.89 LENS", held)
	}
	// Idempotent: a second sweep mints nothing more (once-per-item claim).
	if n2, _ := live.RunOnce(ctx); n2 != 0 {
		t.Fatalf("second sweep must mint 0 (once-per-item), got %d", n2)
	}
}

// (proof 2) WARMUP: below MinUnlinkedGraders pays zero; an all-pass (non-discriminating) item pays zero.
func TestEvalContribution_WarmupAndNonDiscriminating_Integration(t *testing.T) {
	pool, ledger := evalHarness(t)
	ctx := context.Background()
	verifyWorkspace(t, pool, "authorA")
	live := NewEvalContributionMinter(pool, ledger, 10, func() bool { return true })

	// (a) Only 2 distinct graders < 3 → zero (warmup not cleared).
	evalSeedItem(t, pool, "warm", "authorA")
	evalGrade(t, pool, "warm", "wsG1", 1.0)
	evalGrade(t, pool, "warm", "wsG2", 0.0)
	if n, err := live.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("warmup: 2 graders must mint 0: n=%d err=%v", n, err)
	}

	// (b) 3 graders but ALL pass (Var 0) → non-discriminating → zero.
	evalSeedItem(t, pool, "easy", "authorA")
	evalGrade(t, pool, "easy", "wsG1", 1.0)
	evalGrade(t, pool, "easy", "wsG2", 1.0)
	evalGrade(t, pool, "easy", "wsG3", 1.0)
	if n, err := live.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("non-discriminating (all-pass) must mint 0: n=%d err=%v", n, err)
	}
	if n, _, _ := evalMintRows(t, pool, "authorA"); n != 0 {
		t.Fatalf("warmup+non-discriminating: no mint rows, got %d", n)
	}
}

// (proof 6) U6: an UNVERIFIED author mints nothing (the held-ledger verifyEarn floor rolls it back);
// verifying then lets the SAME item mint — isolating the U6 floor as the cause. The credit type is
// TypeEvalContributionHeld, which is in mintTypeList, so the same floor + 24h rate cap apply.
func TestEvalContribution_U6Floor_Integration(t *testing.T) {
	pool, ledger := evalHarness(t)
	ctx := context.Background()
	evalSeedItem(t, pool, "item1", "authorU") // authorU is NOT earn_verified
	evalGrade(t, pool, "item1", "wsG1", 1.0)
	evalGrade(t, pool, "item1", "wsG2", 1.0)
	evalGrade(t, pool, "item1", "wsG3", 0.0)
	live := NewEvalContributionMinter(pool, ledger, 10, func() bool { return true })

	// Unverified → the per-item tx rolls back at verifyEarn; no claim row, no credit, RunOnce reports 0 minted.
	if n, err := live.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("unverified author must mint 0 (U6 floor rolls back): n=%d err=%v", n, err)
	}
	if n, _, _ := evalMintRows(t, pool, "authorU"); n != 0 {
		t.Fatalf("U6: claim row must be rolled back, got %d", n)
	}
	// Verify → the SAME item now mints, proving the U6 floor (not some other gate) was the only blocker.
	verifyWorkspace(t, pool, "authorU")
	if n, err := live.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("verified author must now mint 1: n=%d err=%v", n, err)
	}
	// The ledger row is the gated held mint type.
	var typ string
	if err := pool.QueryRow(ctx, `SELECT type FROM lens_token_ledger WHERE workspace_id='authorU' ORDER BY created_at DESC LIMIT 1`).Scan(&typ); err != nil {
		t.Fatal(err)
	}
	if typ != mining.TypeEvalContributionHeld {
		t.Fatalf("eval mint ledger type = %q, want %q (∈ mintTypeList ⇒ U6 floor + 24h cap)", typ, mining.TypeEvalContributionHeld)
	}
}
