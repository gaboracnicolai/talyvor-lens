package mining

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/earnverify"
)

// heldMint runs one CreditHeldTx (pool_royalty_held) in its own tx — the helper
// for the rate-cap held-path tests.
func heldMint(t *testing.T, pool *pgxpool.Pool, store *LedgerStore, ws string, amount int64) error {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreditHeldTx(ctx, tx, ws, amount, TypePoolRoyaltyHeld, "royalty", nil); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

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
		`CREATE TABLE lxc_purchases (workspace_id TEXT NOT NULL, status TEXT NOT NULL, lxc_amount BIGINT NOT NULL DEFAULT 0)`,
		`CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY,
			balance BIGINT NOT NULL DEFAULT 0,
			held_balance BIGINT NOT NULL DEFAULT 0,
			lifetime_earned BIGINT NOT NULL DEFAULT 0,
			lifetime_spent BIGINT NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_ledger (id BIGSERIAL PRIMARY KEY,
			workspace_id TEXT NOT NULL, amount BIGINT NOT NULL,
			balance_after BIGINT NOT NULL, type TEXT NOT NULL,
			description TEXT, metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE mint_idempotency (request_id TEXT NOT NULL, workspace_id TEXT NOT NULL,
			mint_type TEXT NOT NULL, amount BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (request_id, workspace_id, mint_type))`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool
}

func balanceOf(t *testing.T, pool *pgxpool.Pool, ws string) int64 {
	t.Helper()
	var bal int64
	err := pool.QueryRow(context.Background(), `SELECT balance FROM lens_token_balances WHERE workspace_id=$1`, ws).Scan(&bal)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return bal
}

func heldOf(t *testing.T, pool *pgxpool.Pool, ws string) int64 {
	t.Helper()
	var held int64
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

	if _, err := store.CreditOnce(ctx, "r1", "ws_u", micro(1.0), TypeComputeMineHeld, "compute", nil); !errors.Is(err, ErrEarnNotVerified) {
		t.Fatalf("unverified compute mint must be blocked, got %v", err)
	}
	if b := balanceOf(t, pool, "ws_u"); b != 0 {
		t.Fatalf("blocked mint must leave balance 0, got %v", b)
	}

	mustExec(t, pool, `UPDATE workspaces SET earn_verified=true WHERE id='ws_u'`) // vouch
	if _, err := store.CreditOnce(ctx, "r1", "ws_u", micro(1.0), TypeComputeMineHeld, "compute", nil); err != nil {
		t.Fatalf("verified compute mint must succeed, got %v", err)
	}
	if b := balanceOf(t, pool, "ws_u"); b != micro(1.0) {
		t.Fatalf("verified mint must credit micro(1.0), got %v", b)
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
	if _, err := store.CreditOnce(ctx, "r1", "ws_p", micro(1.0), TypeCacheMine, "cache", nil); err != nil {
		t.Fatalf("a paid workspace must earn, got %v", err)
	}
	if b := balanceOf(t, pool, "ws_p"); b != micro(1.0) {
		t.Fatalf("paid mint = %v, want micro(1.0)", b)
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
	err = store.CreditHeldTx(ctx, tx, "ws_c", micro(5.0), TypePoolRoyaltyHeld, "royalty", nil)
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

	if _, err := store.CreditOnce(ctx, "rid", "ws_v", micro(2.0), TypeCacheMine, "cache", nil); err != nil {
		t.Fatalf("first mint: %v", err)
	}
	already, err := store.CreditOnce(ctx, "rid", "ws_v", micro(2.0), TypeCacheMine, "cache", nil)
	if err != nil || !already {
		t.Fatalf("replay must be suppressed (alreadyMinted): already=%v err=%v", already, err)
	}
	if b := balanceOf(t, pool, "ws_v"); b != micro(2.0) {
		t.Fatalf("DOUBLE-MINT: balance=%v, want micro(2.0) (idempotent — one credit)", b)
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
	if err := store.Credit(ctx, "ws_v", micro(2.0), TypeCacheMine, "cache", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Credit(ctx, "ws_v", micro(2.0), TypeCacheMine, "cache", nil); err != nil {
		t.Fatal(err)
	}
	if b := balanceOf(t, pool, "ws_v"); b != micro(4.0) {
		t.Fatalf("expected the unprotected Credit path to DOUBLE-mint to micro(4.0) (the RED), got %v", b)
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

	if err := store.Credit(ctx, "ws_u", micro(3.0), "unstake", "stake return", nil); err != nil {
		t.Fatalf("conservation (unstake) for an unverified workspace must NOT be gated, got %v", err)
	}
	if b := balanceOf(t, pool, "ws_u"); b != micro(3.0) {
		t.Fatalf("conservation credit = %v, want micro(3.0) (ungated)", b)
	}
}

// ─── U6 PR2: per-identity rate cap ─────────────────────────────────────────

func ratecapStore(t *testing.T, pool *pgxpool.Pool, cap int64) *LedgerStore {
	t.Helper()
	s := NewLedgerStore(pool)
	s.SetMintVerifier(earnverify.New())
	s.SetMintRateCap(cap, 24*time.Hour)
	return s
}

// TestU6RateCap_BlocksAcrossMintTypes — the cap SUMs ALL mint types together (so
// an attacker can't evade by splitting across tracks); the over-cap mint is
// blocked AND rolled back (balance reflects only what fit under the cap).
func TestU6RateCap_BlocksAcrossMintTypes(t *testing.T) {
	pool := u6TestPool(t)
	ctx := context.Background()
	store := ratecapStore(t, pool, micro(10.0)) // 10 LENS / 24h
	mustExec(t, pool, `INSERT INTO workspaces (id, earn_verified) VALUES ('ws_cap', true)`)

	// 8 (compute) + micro(1.5) (cache) = micro(9.5) ≤ 10 → both mint.
	if _, err := store.CreditOnce(ctx, "r1", "ws_cap", micro(8.0), TypeComputeMineHeld, "c", nil); err != nil { // Phase-3: compute_mine_held is the gated mint type
		t.Fatalf("mint 1: %v", err)
	}
	if _, err := store.CreditOnce(ctx, "r2", "ws_cap", micro(1.5), TypeCacheMineHeld, "c", nil); err != nil { // Phase-1: cache_mine_held is the gated mint type
		t.Fatalf("mint 2: %v", err)
	}
	// +micro(1.0) (embedding) = micro(10.5) > 10 → BLOCKED (sums across the three different types).
	if _, err := store.CreditOnce(ctx, "r3", "ws_cap", micro(1.0), TypeEmbeddingMineHeld, "c", nil); !errors.Is(err, ErrMintRateCapExceeded) { // Phase-3: embedding_mine_held
		t.Fatalf("over-cap mint must be blocked across mint types, got %v", err)
	}
	if b := balanceOf(t, pool, "ws_cap"); b != micro(9.5) {
		t.Fatalf("balance=%v, want micro(9.5) (the over-cap mint rolled back)", b)
	}
}

// TestU6RateCap_HeldMintCapped — the held pool-royalty mint (heldInner path) is
// capped too, and counts against the SAME per-workspace ceiling as spendable mints.
func TestU6RateCap_HeldMintCapped(t *testing.T) {
	pool := u6TestPool(t)
	ctx := context.Background()
	store := ratecapStore(t, pool, micro(5.0))
	mustExec(t, pool, `INSERT INTO workspaces (id, earn_verified) VALUES ('ws_h', true)`)

	// 2 (annotation, spendable) + micro(2.5) (held) = micro(4.5) ≤ 5 → both.
	// (annotation is still a DIRECT/spendable mint after Phase-3 moved compute to held.)
	if _, err := store.CreditOnce(ctx, "r1", "ws_h", micro(2.0), TypeAnnotationMine, "c", nil); err != nil {
		t.Fatalf("spendable mint: %v", err)
	}
	if err := heldMint(t, pool, store, "ws_h", micro(2.5)); err != nil {
		t.Fatalf("held mint under cap: %v", err)
	}
	// +1 held = micro(5.5) > 5 → blocked (held counts against the same ceiling as spendable).
	if err := heldMint(t, pool, store, "ws_h", micro(1.0)); !errors.Is(err, ErrMintRateCapExceeded) {
		t.Fatalf("over-cap held mint must be blocked, got %v", err)
	}
	if h := heldOf(t, pool, "ws_h"); h != micro(2.5) {
		t.Fatalf("held=%v, want micro(2.5) (over-cap held mint rolled back)", h)
	}
}

// TestU6RateCap_FinalizeNotDoubleCounted — the critical no-double-count: a held
// mint counts (pool_royalty_held ∈ mintTypeList); FINALIZING it writes a
// pool_royalty row that the SUM EXCLUDES. If finalize were counted, the second
// mint below would be blocked; its success proves the settlement isn't double-counted.
func TestU6RateCap_FinalizeNotDoubleCounted(t *testing.T) {
	pool := u6TestPool(t)
	ctx := context.Background()
	store := ratecapStore(t, pool, micro(10.0))
	mustExec(t, pool, `INSERT INTO workspaces (id, earn_verified) VALUES ('ws_f', true)`)

	// held mint 8 (counts 8 toward the cap).
	if err := heldMint(t, pool, store, "ws_f", micro(8.0)); err != nil {
		t.Fatalf("held mint: %v", err)
	}
	// finalize the 8 (writes a pool_royalty settlement row — NOT a mint type).
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeHeldTx(ctx, tx, "ws_f", micro(8.0), "settle", nil); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("finalize: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// a fresh micro(1.5) mint: counted = 8 (held) + micro(1.5) = micro(9.5) ≤ 10 → OK.
	// (if finalize double-counted, counted would be 16 > 10 → blocked.)
	if _, err := store.CreditOnce(ctx, "r1", "ws_f", micro(1.5), TypeCacheMine, "c", nil); err != nil {
		t.Fatalf("finalize must NOT double-count toward the cap, got %v", err)
	}
}

// TestU6RateCap_OldMintsOutsideWindow — mints older than the rolling window
// don't count, so the cap is per-window not all-time.
func TestU6RateCap_OldMintsOutsideWindow(t *testing.T) {
	pool := u6TestPool(t)
	ctx := context.Background()
	store := ratecapStore(t, pool, micro(10.0))
	mustExec(t, pool, `INSERT INTO workspaces (id, earn_verified) VALUES ('ws_w', true)`)
	// a big mint 48h ago (outside the 24h window) — inserted directly.
	mustExec(t, pool, `INSERT INTO lens_token_balances (workspace_id, balance) VALUES ('ws_w', 100)`)
	mustExec(t, pool, `INSERT INTO lens_token_ledger (workspace_id, amount, balance_after, type, created_at)
		VALUES ('ws_w', 100, 100, 'cache_mine', now() - interval '48 hours')`)
	// a fresh 8-LENS mint succeeds — the old 100 is outside the window.
	if _, err := store.CreditOnce(ctx, "r1", "ws_w", micro(8.0), TypeComputeMine, "c", nil); err != nil {
		t.Fatalf("mints outside the window must not count, got %v", err)
	}
}

// TestU6RateCap_ConservationUngatedAtCap — a workspace AT its mint cap can still
// move existing value (unstake): the cap is mint-only, conservation isn't throttled.
func TestU6RateCap_ConservationUngatedAtCap(t *testing.T) {
	pool := u6TestPool(t)
	ctx := context.Background()
	store := ratecapStore(t, pool, micro(5.0))
	mustExec(t, pool, `INSERT INTO workspaces (id, earn_verified) VALUES ('ws_c', true)`)
	if _, err := store.CreditOnce(ctx, "r1", "ws_c", micro(5.0), TypeComputeMine, "c", nil); err != nil {
		t.Fatalf("mint to cap: %v", err)
	}
	// at the cap, a mint would block — but a conservation credit must still work.
	if err := store.Credit(ctx, "ws_c", micro(100.0), "unstake", "stake return", nil); err != nil {
		t.Fatalf("conservation at the mint cap must NOT be throttled, got %v", err)
	}
	if b := balanceOf(t, pool, "ws_c"); b != micro(105.0) {
		t.Fatalf("balance=%v, want micro(105.0) (5 mint + 100 unstake)", b)
	}
}
