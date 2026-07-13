package mining

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Phase-0 Item B — cache owner-linkage self-deal guard (real PG). A cross-workspace
// cache hit between two workspaces the SAME operator controls must NOT mint the 10×
// cross-workspace rate; it downgrades to the same-workspace rate. The headline case
// is two VOUCHED workspaces (earn_verified, NO cards) declared same-operator via
// workspace_owner_links — the card-fingerprint signal is blind for them, so this
// proves the operator-declared signal covers the closed-test vouch scenario.

type cacheLinkVerifier struct{ verified map[string]bool }

func (v cacheLinkVerifier) MayEarn(_ context.Context, _ pgx.Tx, ws string) (bool, error) {
	return v.verified[ws], nil
}

func cacheLinkHarness(t *testing.T, verified ...string) (*pgxpool.Pool, *CacheMiner) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG cache-linkage test")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "lens_cachelink_test"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP SCHEMA IF EXISTS lens_cachelink_test CASCADE`,
		`CREATE SCHEMA lens_cachelink_test`,
		`CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY, balance BIGINT NOT NULL DEFAULT 0,
			held_balance BIGINT NOT NULL DEFAULT 0, lifetime_earned BIGINT NOT NULL DEFAULT 0,
			lifetime_spent BIGINT NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_ledger (id UUID NOT NULL DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL,
			amount BIGINT NOT NULL, balance_after BIGINT NOT NULL, type TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '', metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (id, workspace_id))`,
		`CREATE TABLE mint_idempotency (request_id TEXT NOT NULL, workspace_id TEXT NOT NULL,
			mint_type TEXT NOT NULL, amount BIGINT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (request_id, workspace_id, mint_type))`,
		`CREATE TABLE workspace_card_fingerprints (workspace_id TEXT NOT NULL, fingerprint_hash TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (workspace_id, fingerprint_hash))`,
		`CREATE TABLE workspace_owner_links (workspace_id TEXT NOT NULL, owner_key TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (workspace_id, owner_key))`,
		`CREATE TABLE traffic_mint_holds (request_id TEXT NOT NULL, workspace_id TEXT NOT NULL, mint_type TEXT NOT NULL,
			minted_amount BIGINT NOT NULL, status TEXT NOT NULL DEFAULT 'held', finalize_after TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (request_id, workspace_id, mint_type))`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	ledger := NewLedgerStore(pool)
	v := cacheLinkVerifier{verified: map[string]bool{}}
	for _, ws := range verified {
		v.verified[ws] = true
	}
	ledger.SetMintVerifier(v) // U6 floor, like prod
	miner := NewCacheMiner(ledger, true)
	miner.SetOwnerLinkageCheck(true) // the guard under test (prod cache stays unwired)
	return pool, miner
}

// cacheBal reads the HELD balance — Phase-1 Item 1: cache mints land HELD, not
// spendable, so the earned rate accrues to held_balance until the finalize sweeper.
func cacheBal(t *testing.T, pool *pgxpool.Pool, ws string) int64 {
	t.Helper()
	var b int64
	_ = pool.QueryRow(context.Background(),
		`SELECT COALESCE((SELECT held_balance FROM lens_token_balances WHERE workspace_id=$1),0)`, ws).Scan(&b)
	return b
}

// cacheSpendable reads the SPENDABLE balance (0 until finalize).
func cacheSpendable(t *testing.T, pool *pgxpool.Pool, ws string) int64 {
	t.Helper()
	var b int64
	_ = pool.QueryRow(context.Background(),
		`SELECT COALESCE((SELECT balance FROM lens_token_balances WHERE workspace_id=$1),0)`, ws).Scan(&b)
	return b
}

// RED: two VOUCHED workspaces (no cards) declared same operator → a cross-workspace
// hit is self-dealing and must downgrade to the same-workspace rate. Before the fix
// it mints the 10× cross rate to A.
func TestCacheHit_OwnerLinkedVouched_DowngradedFromCrossRate_Integration(t *testing.T) {
	pool, miner := cacheLinkHarness(t, "wsA") // wsA verified-to-earn (vouched)
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO workspace_owner_links (workspace_id, owner_key) VALUES ('wsA','human-1'),('wsB','human-1')`); err != nil {
		t.Fatal(err)
	}
	if err := miner.RecordCacheHit(ctx, "wsA", "wsB", "exact", "req-selfdeal"); err != nil {
		t.Fatalf("RecordCacheHit: %v", err)
	}
	if got := cacheBal(t, pool, "wsA"); got != CacheHitSameWorkspace {
		t.Fatalf("wsA credited %d µLENS, want %d (same-workspace) — a self-deal between owner-linked VOUCHED "+
			"workspaces must NOT mint the %d cross-workspace rate", got, CacheHitSameWorkspace, CacheHitCrossWorkspace)
	}
}

// The carded case (parity with pool royalty): shared card fingerprint → downgraded too.
func TestCacheHit_OwnerLinkedByCard_Downgraded_Integration(t *testing.T) {
	pool, miner := cacheLinkHarness(t, "wsA")
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO workspace_card_fingerprints (workspace_id, fingerprint_hash) VALUES ('wsA','fp-1'),('wsB','fp-1')`); err != nil {
		t.Fatal(err)
	}
	if err := miner.RecordCacheHit(ctx, "wsA", "wsB", "exact", "req-card"); err != nil {
		t.Fatal(err)
	}
	if got := cacheBal(t, pool, "wsA"); got != CacheHitSameWorkspace {
		t.Fatalf("wsA credited %d, want %d (shared card fingerprint = same operator)", got, CacheHitSameWorkspace)
	}
}

// CONTROL / no-over-block: genuinely independent operators (no owner_key, no shared
// card) still get the full cross rate. Passes before AND after the fix.
func TestCacheHit_UnlinkedWorkspaces_StillGetCrossRate_Integration(t *testing.T) {
	pool, miner := cacheLinkHarness(t, "wsA")
	ctx := context.Background()
	if err := miner.RecordCacheHit(ctx, "wsA", "wsB", "exact", "req-honest"); err != nil {
		t.Fatal(err)
	}
	if got := cacheBal(t, pool, "wsA"); got != CacheHitCrossWorkspace {
		t.Fatalf("wsA credited %d, want %d — an HONEST cross-workspace hit (unlinked) must still get the cross rate (no over-block)",
			got, CacheHitCrossWorkspace)
	}
}

// Phase-1 Item 1 + Item 3 RED PROOF: a cache mint lands HELD (not spendable) and
// becomes spendable ONLY after the window + finalize. Before the fix it landed
// SPENDABLE immediately (cacheSpendable would be the rate, not 0). Also proves the
// finalize sweeper settles it and GetTotalSupply counts the finalized cache_mine.
func TestCacheHit_MintsHeld_FinalizesAfterWindow_Integration(t *testing.T) {
	pool, miner := cacheLinkHarness(t, "wsA")
	ctx := context.Background()
	miner.SetHoldbackWindow(time.Millisecond) // due almost immediately for the sweep

	if err := miner.RecordCacheHit(ctx, "wsA", "wsB", "exact", "req-held-1"); err != nil {
		t.Fatalf("RecordCacheHit: %v", err)
	}
	if held := cacheBal(t, pool, "wsA"); held != CacheHitCrossWorkspace {
		t.Fatalf("wsA held=%d, want %d (mint must land HELD)", held, CacheHitCrossWorkspace)
	}
	if sp := cacheSpendable(t, pool, "wsA"); sp != 0 {
		t.Fatalf("wsA spendable=%d, want 0 — a held mint is NOT spendable before finalize (was: minted spendable immediately)", sp)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM traffic_mint_holds WHERE request_id='req-held-1'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "held" {
		t.Fatalf("hold status=%q, want held", status)
	}

	time.Sleep(5 * time.Millisecond)
	sw := NewTrafficMintSweeper(pool, miner.ledger)
	if n, err := sw.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("sweep finalized n=%d err=%v, want 1", n, err)
	}
	if sp := cacheSpendable(t, pool, "wsA"); sp != CacheHitCrossWorkspace {
		t.Fatalf("wsA spendable after finalize=%d, want %d", sp, CacheHitCrossWorkspace)
	}
	if held := cacheBal(t, pool, "wsA"); held != 0 {
		t.Fatalf("wsA held after finalize=%d, want 0", held)
	}
	if supply, _ := miner.ledger.GetTotalSupply(ctx); supply != CacheHitCrossWorkspace {
		t.Fatalf("supply=%d, want %d (finalized mint counted as cache_mine)", supply, CacheHitCrossWorkspace)
	}
}

// The CLAWBACK surface Phase 2 needs: RevokeHeldTxAs reverses a held mint before finalize.
func TestCacheHit_HeldMint_RevocableBeforeFinalize_Integration(t *testing.T) {
	pool, miner := cacheLinkHarness(t, "wsA")
	ctx := context.Background()
	miner.SetHoldbackWindow(time.Hour) // stays held

	if err := miner.RecordCacheHit(ctx, "wsA", "wsB", "exact", "req-revoke-1"); err != nil {
		t.Fatal(err)
	}
	if held := cacheBal(t, pool, "wsA"); held != CacheHitCrossWorkspace {
		t.Fatalf("pre-revoke held=%d, want %d", held, CacheHitCrossWorkspace)
	}
	tx, _ := pool.Begin(ctx)
	if _, err := tx.Exec(ctx, `UPDATE traffic_mint_holds SET status='revoked' WHERE request_id='req-revoke-1' AND status='held'`); err != nil {
		t.Fatal(err)
	}
	if err := miner.ledger.RevokeHeldTxAs(ctx, tx, "wsA", CacheHitCrossWorkspace, TypePoolRoyaltyRevoked, "revoked: gamed", nil); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit(ctx)
	if held := cacheBal(t, pool, "wsA"); held != 0 {
		t.Fatalf("post-revoke held=%d, want 0 (burned)", held)
	}
	if sp := cacheSpendable(t, pool, "wsA"); sp != 0 {
		t.Fatalf("post-revoke spendable=%d, want 0", sp)
	}
	if supply, _ := miner.ledger.GetTotalSupply(ctx); supply != 0 {
		t.Fatalf("post-revoke supply=%d, want 0 (a revoked held mint never entered supply)", supply)
	}
}
