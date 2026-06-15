package mining

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/earnverify"
)

// u6TestPool builds a real-PG pool with the ledger + idempotency + verify-signal
// tables the U6 chokepoint touches end-to-end. Skips without LENS_TEST_DATABASE_URL.
func u6TestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG U6 chokepoint test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS lens_token_ledger`,
		`DROP TABLE IF EXISTS lens_token_balances`,
		`DROP TABLE IF EXISTS mint_idempotency`,
		`DROP TABLE IF EXISTS lxc_purchases`,
		`DROP TABLE IF EXISTS workspaces`,
		`CREATE TABLE workspaces (id TEXT PRIMARY KEY, earn_verified BOOLEAN NOT NULL DEFAULT false)`,
		`CREATE TABLE lxc_purchases (workspace_id TEXT NOT NULL, status TEXT NOT NULL, lxc_amount DOUBLE PRECISION NOT NULL DEFAULT 0)`,
		`CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY,
			balance DOUBLE PRECISION NOT NULL DEFAULT 0,
			held_balance DOUBLE PRECISION NOT NULL DEFAULT 0,
			lifetime_earned DOUBLE PRECISION NOT NULL DEFAULT 0,
			lifetime_spent DOUBLE PRECISION NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_ledger (id BIGSERIAL PRIMARY KEY,
			workspace_id TEXT NOT NULL, amount DOUBLE PRECISION NOT NULL,
			balance_after DOUBLE PRECISION NOT NULL, type TEXT NOT NULL,
			description TEXT, metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE mint_idempotency (request_id TEXT NOT NULL, workspace_id TEXT NOT NULL,
			mint_type TEXT NOT NULL, amount DOUBLE PRECISION NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (request_id, workspace_id, mint_type))`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool
}

func balanceOf(t *testing.T, pool *pgxpool.Pool, ws string) float64 {
	t.Helper()
	var bal float64
	err := pool.QueryRow(context.Background(), `SELECT balance FROM lens_token_balances WHERE workspace_id=$1`, ws).Scan(&bal)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return bal
}

func heldOf(t *testing.T, pool *pgxpool.Pool, ws string) float64 {
	t.Helper()
	var held float64
	err := pool.QueryRow(context.Background(), `SELECT held_balance FROM lens_token_balances WHERE workspace_id=$1`, ws).Scan(&held)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return held
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec: %v", err)
	}
}

// TestU6Chokepoint_BlocksUnverified_AllowsVerified — the headline: with the real
// earnverify gate wired, an UNVERIFIED workspace mints NOTHING through the
// ledger chokepoint (balance unchanged, ErrEarnNotVerified); after an admin
// vouch it mints. Proves no value reaches an unverified identity.
func TestU6Chokepoint_BlocksUnverified_AllowsVerified(t *testing.T) {
	pool := u6TestPool(t)
	ctx := context.Background()
	store := NewLedgerStore(pool)
	store.SetMintVerifier(earnverify.New())
	mustExec(t, pool, `INSERT INTO workspaces (id) VALUES ('ws_u')`) // unverified

	if _, err := store.CreditOnce(ctx, "r1", "ws_u", 1.0, TypeComputeMine, "compute", nil); !errors.Is(err, ErrEarnNotVerified) {
		t.Fatalf("unverified compute mint must be blocked, got %v", err)
	}
	if b := balanceOf(t, pool, "ws_u"); b != 0 {
		t.Fatalf("blocked mint must leave balance 0, got %v", b)
	}

	mustExec(t, pool, `UPDATE workspaces SET earn_verified=true WHERE id='ws_u'`) // vouch
	if _, err := store.CreditOnce(ctx, "r1", "ws_u", 1.0, TypeComputeMine, "compute", nil); err != nil {
		t.Fatalf("verified compute mint must succeed, got %v", err)
	}
	if b := balanceOf(t, pool, "ws_u"); b != 1.0 {
		t.Fatalf("verified mint must credit 1.0, got %v", b)
	}
}

// TestU6Chokepoint_CompletedPurchaseEarns — a completed lxc_purchase verifies
// without any column write (read-time derive).
func TestU6Chokepoint_CompletedPurchaseEarns(t *testing.T) {
	pool := u6TestPool(t)
	ctx := context.Background()
	store := NewLedgerStore(pool)
	store.SetMintVerifier(earnverify.New())
	mustExec(t, pool, `INSERT INTO workspaces (id) VALUES ('ws_p')`)
	mustExec(t, pool, `INSERT INTO lxc_purchases (workspace_id, status, lxc_amount) VALUES ('ws_p','completed',100)`)
	if _, err := store.CreditOnce(ctx, "r1", "ws_p", 1.0, TypeCacheMine, "cache", nil); err != nil {
		t.Fatalf("a paid workspace must earn, got %v", err)
	}
	if b := balanceOf(t, pool, "ws_p"); b != 1.0 {
		t.Fatalf("paid mint = %v, want 1.0", b)
	}
}

// TestU6Chokepoint_HeldMintBlocksUnverified — the worst Sybil hole: the
// pool-royalty HELD mint (CreditHeldTx, pool_royalty_held) is gated too. An
// unverified contributor accrues no held royalty.
func TestU6Chokepoint_HeldMintBlocksUnverified(t *testing.T) {
	pool := u6TestPool(t)
	ctx := context.Background()
	store := NewLedgerStore(pool)
	store.SetMintVerifier(earnverify.New())
	mustExec(t, pool, `INSERT INTO workspaces (id) VALUES ('ws_c')`) // unverified contributor

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = store.CreditHeldTx(ctx, tx, "ws_c", 5.0, TypePoolRoyaltyHeld, "royalty", nil)
	_ = tx.Rollback(ctx)
	if !errors.Is(err, ErrEarnNotVerified) {
		t.Fatalf("unverified held royalty mint must be blocked, got %v", err)
	}
	if h := heldOf(t, pool, "ws_c"); h != 0 {
		t.Fatalf("blocked held mint must leave held_balance 0, got %v", h)
	}
}

// TestU6Chokepoint_IdempotentNoDoubleMint — GREEN: replaying the same
// (request_id, workspace, mint_type) credits ONCE; the second call is suppressed.
func TestU6Chokepoint_IdempotentNoDoubleMint(t *testing.T) {
	pool := u6TestPool(t)
	ctx := context.Background()
	store := NewLedgerStore(pool)
	store.SetMintVerifier(earnverify.New())
	mustExec(t, pool, `INSERT INTO workspaces (id, earn_verified) VALUES ('ws_v', true)`)

	if _, err := store.CreditOnce(ctx, "rid", "ws_v", 2.0, TypeCacheMine, "cache", nil); err != nil {
		t.Fatalf("first mint: %v", err)
	}
	already, err := store.CreditOnce(ctx, "rid", "ws_v", 2.0, TypeCacheMine, "cache", nil)
	if err != nil || !already {
		t.Fatalf("replay must be suppressed (alreadyMinted): already=%v err=%v", already, err)
	}
	if b := balanceOf(t, pool, "ws_v"); b != 2.0 {
		t.Fatalf("DOUBLE-MINT: balance=%v, want 2.0 (idempotent — one credit)", b)
	}
}

// TestU6Chokepoint_PlainCreditDoubleMints_RED — the contrast that proves the
// idempotency matters: the OLD unprotected path (plain Credit, still used by
// conservation) DOUBLE-mints on replay — exactly the hole CreditOnce closes for
// the mint tracks. (Verified workspace so the gate isn't what stops it.)
func TestU6Chokepoint_PlainCreditDoubleMints_RED(t *testing.T) {
	pool := u6TestPool(t)
	ctx := context.Background()
	store := NewLedgerStore(pool)
	store.SetMintVerifier(earnverify.New())
	mustExec(t, pool, `INSERT INTO workspaces (id, earn_verified) VALUES ('ws_v', true)`)

	// plain Credit has no claim row — two identical credits both land.
	if err := store.Credit(ctx, "ws_v", 2.0, TypeCacheMine, "cache", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Credit(ctx, "ws_v", 2.0, TypeCacheMine, "cache", nil); err != nil {
		t.Fatal(err)
	}
	if b := balanceOf(t, pool, "ws_v"); b != 4.0 {
		t.Fatalf("expected the unprotected Credit path to DOUBLE-mint to 4.0 (the RED), got %v", b)
	}
}

// TestU6Chokepoint_ConservationUngated — a conservation credit (unstake) by an
// UNVERIFIED workspace still works: it moves existing value, it is not a mint,
// so the gate does not touch it. The exact invariant — money-movement is never
// gated.
func TestU6Chokepoint_ConservationUngated(t *testing.T) {
	pool := u6TestPool(t)
	ctx := context.Background()
	store := NewLedgerStore(pool)
	store.SetMintVerifier(earnverify.New())
	mustExec(t, pool, `INSERT INTO workspaces (id) VALUES ('ws_u')`) // unverified

	if err := store.Credit(ctx, "ws_u", 3.0, "unstake", "stake return", nil); err != nil {
		t.Fatalf("conservation (unstake) for an unverified workspace must NOT be gated, got %v", err)
	}
	if b := balanceOf(t, pool, "ws_u"); b != 3.0 {
		t.Fatalf("conservation credit = %v, want 3.0 (ungated)", b)
	}
}
