package poolroyalty

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/mining"
)

// L·seed money-safety proof: a cache/distill entry OWNED by economy.TalyvorSeedWorkspace, when
// served + minted-against, credits exactly ZERO on BOTH royalty paths — because the seed owner is
// never earn_verified, so MayEarn is false and the held-ledger verifyEarn chokepoint rolls the
// mint back. The CONTRAST (a verified contributor in the identical scenario mints > 0) isolates
// MayEarn as the cause (not self-serve, not a key mismatch). Real-PG; the ledger is wired exactly
// like production (SetMintVerifier(earnverify.New())).

func seedZeroMintHarness(t *testing.T) (*pgxpool.Pool, *mining.LedgerStore) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG seed zero-mint test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP VIEW IF EXISTS distill_royalty_margin`,
		`DROP VIEW IF EXISTS pool_royalty_margin`,
		`DROP TABLE IF EXISTS lens_token_ledger`,
		`DROP TABLE IF EXISTS lens_token_balances`,
		`DROP TABLE IF EXISTS pool_royalty_mints`,
		`DROP TABLE IF EXISTS distill_royalty_mints`,
		`DROP TABLE IF EXISTS distill_royalty_basis`,
		`DROP TABLE IF EXISTS workspaces`,
		`DROP TABLE IF EXISTS lxc_purchases`,
		`CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY, balance BIGINT NOT NULL DEFAULT 0,
			held_balance BIGINT NOT NULL DEFAULT 0, lifetime_earned BIGINT NOT NULL DEFAULT 0,
			lifetime_spent BIGINT NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_ledger (id UUID NOT NULL DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL,
			amount BIGINT NOT NULL, balance_after BIGINT NOT NULL, type TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '', metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (id, workspace_id))`,
		`CREATE TABLE pool_royalty_mints (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), request_id TEXT NOT NULL UNIQUE,
			requester_workspace_id TEXT NOT NULL, contributor_workspace_id TEXT NOT NULL, layer TEXT NOT NULL,
			entry_id TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '',
			similarity DOUBLE PRECISION NOT NULL DEFAULT 0, avoided_cogs_usd DOUBLE PRECISION NOT NULL DEFAULT 0,
			minted_amount BIGINT NOT NULL DEFAULT 0, answer_sha256 TEXT NOT NULL DEFAULT '',
			prompt_sha256 TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'final', finalize_after TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE distill_royalty_basis (owner_workspace_id TEXT NOT NULL, requester_workspace_id TEXT NOT NULL,
			content_hash TEXT NOT NULL, avoided_cogs_usd DOUBLE PRECISION NOT NULL, settled_charge_usd DOUBLE PRECISION,
			vision_model TEXT NOT NULL, vision_input_tokens INTEGER NOT NULL, vision_output_tokens INTEGER NOT NULL,
			captured_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (owner_workspace_id, requester_workspace_id, content_hash))`,
		`CREATE TABLE distill_royalty_mints (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), request_id TEXT NOT NULL UNIQUE,
			contributor_workspace_id TEXT NOT NULL, requester_workspace_id TEXT NOT NULL, content_hash TEXT NOT NULL,
			avoided_cogs_usd DOUBLE PRECISION NOT NULL, minted_amount BIGINT NOT NULL,
			status TEXT NOT NULL DEFAULT 'held', finalize_after TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE workspaces (id TEXT PRIMARY KEY, earn_verified BOOLEAN NOT NULL DEFAULT false)`,
		`CREATE TABLE lxc_purchases (workspace_id TEXT NOT NULL, status TEXT NOT NULL, lxc_amount BIGINT NOT NULL)`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	ledger := mining.NewLedgerStore(pool)
	ledger.SetMintVerifier(earnverify.New()) // U6 floor — wired unconditionally, as production
	return pool, ledger
}

func cacheMintRows(t *testing.T, pool *pgxpool.Pool, contributor string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pool_royalty_mints WHERE contributor_workspace_id=$1`, contributor).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// (1)+(3) CACHE path — a seed-owned served hit mints ZERO; a verified contributor mints > 0.
func TestSeed_CacheZeroMint_Integration(t *testing.T) {
	pool, ledger := seedZeroMintHarness(t)
	ctx := context.Background()
	m := NewMinter(pool, ledger, 0.5, func() bool { return true })

	hit := func(reqID, contributor string) (Result, error) {
		return m.MintServedHit(ctx, ServedHit{
			RequestID: reqID, RequesterWorkspace: "tenantW", ContributorWorkspace: contributor,
			Layer: "exact", Provider: "openai", Model: "gpt-4o", AvoidedCOGSUSD: 1.0,
			AnswerSHA256: "ans", PromptSHA256: "prm", // non-empty: passes the adjudicability gate, isolating MayEarn
		})
	}

	// SEED owner (never earn_verified) → ZERO, and the cause is the U6 MayEarn floor.
	res, err := hit("r-seed", economy.TalyvorSeedWorkspace)
	if !errors.Is(err, mining.ErrEarnNotVerified) {
		t.Fatalf("seed mint must roll back via the U6 floor (ErrEarnNotVerified); got res=%+v err=%v", res, err)
	}
	if res.Minted {
		t.Error("seed must NOT mint")
	}
	if _, held := balances(t, pool, economy.TalyvorSeedWorkspace); held != 0 {
		t.Errorf("seed held_balance %v, want 0", held)
	}
	if n := cacheMintRows(t, pool, economy.TalyvorSeedWorkspace); n != 0 {
		t.Errorf("seed pool_royalty_mints rows %d, want 0 (tx rolled back)", n)
	}

	// CONTRAST: a VERIFIED contributor in the identical scenario mints > 0 → MayEarn is the cause.
	verifyWorkspace(t, pool, "verifiedC")
	res2, err2 := hit("r-verified", "verifiedC")
	if err2 != nil || !res2.Minted || res2.Amount <= 0 {
		t.Fatalf("verified contributor must mint > 0; got res=%+v err=%v", res2, err2)
	}
	if _, held := balances(t, pool, "verifiedC"); held <= 0 {
		t.Errorf("verified held_balance %v, want > 0", held)
	}
}

// (2)+(3) DISTILL path — a seed-owned basis mints ZERO; a verified owner mints > 0 (one RunOnce).
func TestSeed_DistillZeroMint_Integration(t *testing.T) {
	pool, ledger := seedZeroMintHarness(t)
	ctx := context.Background()
	dm := NewDistillMinter(pool, ledger, 0.5, func() bool { return true })

	seedBasis(t, pool, economy.TalyvorSeedWorkspace, "tenantW", "hash-seed", 1.0)
	verifyWorkspace(t, pool, "verifiedC")
	seedBasis(t, pool, "verifiedC", "tenantW", "hash-verified", 1.0)

	if _, err := dm.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if n := mintRowCount(t, pool, economy.TalyvorSeedWorkspace); n != 0 {
		t.Errorf("seed distill mint rows %d, want 0", n)
	}
	if _, held := balances(t, pool, economy.TalyvorSeedWorkspace); held != 0 {
		t.Errorf("seed distill held_balance %v, want 0", held)
	}
	if n := mintRowCount(t, pool, "verifiedC"); n != 1 {
		t.Errorf("verified distill mint rows %d, want 1 (proves MayEarn, not a key mismatch, gates the seed)", n)
	}
	if _, held := balances(t, pool, "verifiedC"); held <= 0 {
		t.Errorf("verified distill held_balance %v, want > 0", held)
	}
}

// (4) INVARIANT — on a fresh DB the seed owner is never earn_verified and has no lxc_purchase, so
// MayEarn returns false. This single fact is what the whole zero-mint guarantee rests on.
func TestSeed_NeverVerified_MayEarnFalse_Integration(t *testing.T) {
	pool, _ := seedZeroMintHarness(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ok, err := earnverify.New().MayEarn(ctx, tx, economy.TalyvorSeedWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatalf("MayEarn(%q) = true — the seed owner must NEVER be verified-to-earn", economy.TalyvorSeedWorkspace)
	}
	if economy.TalyvorSeedWorkspace == economy.TalyvorWorkspace {
		t.Error("the seed workspace must be DISTINCT from the marketplace-fee workspace (isolation)")
	}
}
