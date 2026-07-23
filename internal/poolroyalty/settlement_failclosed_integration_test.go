package poolroyalty

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/mining"
)

// Phase-3 Item 3 — settlement-side fail-closed. Today the finalize sweeper settles
// any due status='held' row, so a held mint the detector NEVER examined (detector
// down / lagging / the 72h window closed first) becomes spendable un-adjudicated.
// The fix (Phase-2's ready design): a `cleared` status; the adjudicator promotes
// non-flagged held→cleared; the sweeper's settle-status becomes flag-driven —
// `held` when OFF (byte-identical) and `cleared` when ON. An un-adjudicated `held`
// row then never finalizes → fail-closed.

func mintStatusOf(t *testing.T, pool *pgxpool.Pool, requestID string) string {
	t.Helper()
	var st string
	_ = pool.QueryRow(context.Background(), `SELECT status FROM pool_royalty_mints WHERE request_id=$1`, requestID).Scan(&st)
	return st
}

// RED: with the fail-closed layer ON (sweeper requires 'cleared'), an
// un-adjudicated held mint must NOT settle — it holds. Before the settle-status
// field existed the sweeper settled 'held' regardless, so this un-adjudicated mint
// became spendable.
func TestFinalizeSweeper_FailClosed_UnadjudicatedHeldDoesNotSettle_Integration(t *testing.T) {
	pool := revokerTestPool(t)
	ctx := context.Background()
	ledger := mining.NewLedgerStore(pool)
	m := NewMinter(pool, ledger, 0.5, func() bool { return true })
	m.SetHoldbackWindow(time.Millisecond) // due almost immediately
	rid := seedHeldMint(t, m, ctx, "fc-unadj", "wsA", 1.0)
	time.Sleep(4 * time.Millisecond)

	sw := NewFinalizeSweeper(pool, ledger, "pool_royalty_mints")
	sw.SetSettleStatus("cleared") // fail-closed ON: ONLY adjudicated-clean (cleared) rows settle

	n, err := sw.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 0 {
		t.Fatalf("fail-closed: an un-adjudicated HELD mint must NOT settle, but %d settled", n)
	}
	if st := mintStatusOf(t, pool, rid); st != "held" {
		t.Fatalf("status=%q, want held (un-adjudicated mint holds, never becomes spendable)", st)
	}
	var spendable, held int64
	mustScan(t, pool, `SELECT balance, held_balance FROM lens_token_balances WHERE workspace_id='wsA'`, &spendable, &held)
	if spendable != 0 {
		t.Fatalf("spendable=%d, want 0 (a mint we could not adjudicate must never become spendable)", spendable)
	}
	if held != mining.FloatToMicroFloor(10.0) {
		t.Fatalf("held=%d, want 10 LENS in µLENS (the mint is held, not burned)", held)
	}
}

// GREEN companion: once the adjudicator has cleared a row (held→cleared), the
// fail-closed sweeper DOES settle it — legitimate mints still flow.
func TestFinalizeSweeper_FailClosed_ClearedSettles_Integration(t *testing.T) {
	pool := revokerTestPool(t)
	ctx := context.Background()
	ledger := mining.NewLedgerStore(pool)
	m := NewMinter(pool, ledger, 0.5, func() bool { return true })
	m.SetHoldbackWindow(time.Millisecond)
	rid := seedHeldMint(t, m, ctx, "fc-clear", "wsA", 1.0)
	// the adjudicator's clear step: non-flagged held → cleared.
	if _, err := pool.Exec(ctx, `UPDATE pool_royalty_mints SET status='cleared' WHERE request_id=$1`, rid); err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * time.Millisecond)

	sw := NewFinalizeSweeper(pool, ledger, "pool_royalty_mints")
	sw.SetSettleStatus("cleared")
	n, err := sw.RunOnce(ctx)
	if err != nil || n != 1 {
		t.Fatalf("cleared mint must settle: n=%d err=%v, want 1", n, err)
	}
	if st := mintStatusOf(t, pool, rid); st != "final" {
		t.Fatalf("status=%q, want final (cleared→final at settlement)", st)
	}
	var spendable int64
	mustScan(t, pool, `SELECT balance FROM lens_token_balances WHERE workspace_id='wsA'`, &spendable)
	if spendable != mining.FloatToMicroFloor(10.0) {
		t.Fatalf("spendable=%d, want 10 LENS in µLENS (a cleared mint settles)", spendable)
	}
}

// Byte-identical when OFF: the DEFAULT sweeper (no SetSettleStatus) settles 'held'
// exactly as before — today's behavior is unchanged unless fail-closed is armed.
func TestFinalizeSweeper_Default_SettlesHeld_ByteIdentical_Integration(t *testing.T) {
	pool := revokerTestPool(t)
	ctx := context.Background()
	ledger := mining.NewLedgerStore(pool)
	m := NewMinter(pool, ledger, 0.5, func() bool { return true })
	m.SetHoldbackWindow(time.Millisecond)
	_ = seedHeldMint(t, m, ctx, "fc-default", "wsA", 1.0)
	time.Sleep(4 * time.Millisecond)

	sw := NewFinalizeSweeper(pool, ledger, "pool_royalty_mints") // default: settles 'held'
	n, err := sw.RunOnce(ctx)
	if err != nil || n != 1 {
		t.Fatalf("default sweeper must settle a due held mint: n=%d err=%v, want 1", n, err)
	}
}
