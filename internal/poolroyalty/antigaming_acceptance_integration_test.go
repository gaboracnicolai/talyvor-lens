package poolroyalty

import (
	"context"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/mining"
)

// TestAntiGaming_Acceptance_RingClawedBackBeforeSettlement is THE spec acceptance
// test (Item 4): a seeded self-dealing ring is DETECTED and CLAWED BACK BEFORE
// settlement, while honest cross-tenant reuse settles normally. Standing
// integration test on real PG — the proof the moat exists.
func TestAntiGaming_Acceptance_RingClawedBackBeforeSettlement(t *testing.T) {
	pool := linkageTestPool(t)
	ctx := context.Background()
	// The audit table the durable Adjudicate path writes (0048).
	linkExec(t, pool, `CREATE TABLE IF NOT EXISTS pool_royalty_adjudications (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(), flag_type TEXT NOT NULL,
		resolution_label TEXT NOT NULL, candidate_request_ids TEXT[] NOT NULL,
		revoked_request_ids TEXT[] NOT NULL, decided_by TEXT NOT NULL, outcome JSONB,
		decided_at TIMESTAMPTZ NOT NULL DEFAULT now())`)
	linkExec(t, pool, `TRUNCATE pool_royalty_adjudications`)

	ledger := mining.NewLedgerStore(pool)
	m := NewMinter(pool, ledger, 0.5, func() bool { return true })
	m.SetHoldbackWindow(time.Millisecond) // finalize_after ≈ now → the sweeper CAN settle a held row

	// The ring: circular reuse among one operator's A, B, C.
	ringAB := seedHeldFor(t, m, ctx, "acc-ab", "wsA", "wsB")
	ringBC := seedHeldFor(t, m, ctx, "acc-bc", "wsB", "wsC")
	ringCA := seedHeldFor(t, m, ctx, "acc-ca", "wsC", "wsA")
	// Honest cross-tenant reuse: different operators.
	honest := seedHeldFor(t, m, ctx, "acc-hon", "wsP", "wsQ")

	// TRANSITIVE identity: A-B via k1, B-C via k2 (A,C not directly linked).
	linkExec(t, pool, `INSERT INTO workspace_owner_links (workspace_id, owner_key) VALUES
		('wsA','acc-k1'), ('wsB','acc-k1'), ('wsB','acc-k2'), ('wsC','acc-k2')`)

	// Pre: each contributor holds 1 LENS held, nothing spendable.
	for _, ws := range []string{"wsA", "wsB", "wsC", "wsP"} {
		var held, bal int64
		if err := pool.QueryRow(ctx, `SELECT held_balance, balance FROM lens_token_balances WHERE workspace_id=$1`, ws).Scan(&held, &bal); err != nil {
			t.Fatal(err)
		}
		if held != micro(1) || bal != 0 {
			t.Fatalf("pre %s: held=%v bal=%v, want 1/0", ws, held, bal)
		}
	}

	// ── ANTI-GAMING RUNS (during the window, BEFORE settlement) ──
	detector := NewRingDetector(pool, "pool_royalty_mints")
	revoker := NewRevoker(pool, ledger)
	adjWriter := NewAdjudicationWriter(pool, revoker)
	auto := NewAutoAdjudicator(detector, adjWriter, func() bool { return true }, 24*time.Hour)

	revoked, err := auto.RunOnce(ctx)
	if err != nil {
		t.Fatalf("auto-adjudicate: %v", err)
	}
	if revoked != 3 {
		t.Fatalf("revoked=%d, want 3 (the whole ring, including the transitive C→A mint)", revoked)
	}

	// The ring's mints are clawed back: status revoked, held burned to 0, NOTHING spendable.
	for _, id := range []string{ringAB, ringBC, ringCA} {
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM pool_royalty_mints WHERE request_id=$1`, id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "revoked" {
			t.Errorf("ring mint %s status=%q, want revoked", id, status)
		}
	}
	for _, ws := range []string{"wsA", "wsB", "wsC"} {
		var held, bal int64
		if err := pool.QueryRow(ctx, `SELECT held_balance, balance FROM lens_token_balances WHERE workspace_id=$1`, ws).Scan(&held, &bal); err != nil {
			t.Fatal(err)
		}
		if held != 0 || bal != 0 {
			t.Errorf("ring ws %s after clawback: held=%v bal=%v, want 0/0 (clawed back, never spendable)", ws, held, bal)
		}
	}
	// The honest mint is untouched — still held, still revocable.
	var honestStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM pool_royalty_mints WHERE request_id=$1`, honest).Scan(&honestStatus); err != nil {
		t.Fatal(err)
	}
	if honestStatus != "held" {
		t.Errorf("honest mint status=%q, want held (must NOT be clawed back)", honestStatus)
	}

	// ── SETTLEMENT RUNS (window elapsed) ──
	time.Sleep(5 * time.Millisecond)
	sweeper := NewFinalizeSweeper(pool, ledger, "pool_royalty_mints")
	n, err := sweeper.RunOnce(ctx)
	if err != nil {
		t.Fatalf("finalize sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("finalized=%d, want 1 (only the honest mint settles; the ring was clawed back BEFORE settlement)", n)
	}

	// The honest contributor now has spendable LENS; the ring operator has NONE.
	var pHeld, pBal int64
	mustScan(t, pool, `SELECT held_balance, balance FROM lens_token_balances WHERE workspace_id='wsP'`, &pHeld, &pBal)
	if pHeld != 0 || pBal != micro(1) {
		t.Errorf("honest wsP after settle: held=%v bal=%v, want 0/1 (settled)", pHeld, pBal)
	}
	for _, ws := range []string{"wsA", "wsB", "wsC"} {
		var bal int64
		if err := pool.QueryRow(ctx, `SELECT balance FROM lens_token_balances WHERE workspace_id=$1`, ws).Scan(&bal); err != nil {
			t.Fatal(err)
		}
		if bal != 0 {
			t.Errorf("ring ws %s spendable=%v after settlement, want 0 — the ring must NEVER become spendable", ws, bal)
		}
	}
	// Supply counts ONLY the honest mint.
	supply, _ := ledger.GetTotalSupply(ctx)
	if supply != micro(1) {
		t.Errorf("total supply=%v, want 1 LENS (only the honest mint entered supply)", supply)
	}
}
