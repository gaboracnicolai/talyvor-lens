package mining

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func singlePartyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG single-party guard test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS traffic_mint_holds, lens_token_balances, lens_token_ledger`,
		`CREATE TABLE traffic_mint_holds (request_id TEXT NOT NULL, workspace_id TEXT NOT NULL, mint_type TEXT NOT NULL,
			minted_amount BIGINT NOT NULL, status TEXT NOT NULL DEFAULT 'held', finalize_after TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (request_id, workspace_id, mint_type))`,
		`CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY, balance BIGINT NOT NULL DEFAULT 0,
			held_balance BIGINT NOT NULL DEFAULT 0, lifetime_earned BIGINT NOT NULL DEFAULT 0,
			lifetime_spent BIGINT NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_ledger (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL,
			amount BIGINT NOT NULL, balance_after BIGINT NOT NULL, type TEXT NOT NULL,
			description TEXT, metadata JSONB, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool
}

// seedPatternHold writes a held pattern mint (both the ledger held credit and the
// traffic_mint_holds row) so a settle actually moves real µLENS.
func seedPatternHold(t *testing.T, pool *pgxpool.Pool, ledger *LedgerStore, reqID, ws string) {
	t.Helper()
	ctx := context.Background()
	tx, _ := pool.Begin(ctx)
	if err := ledger.CreditHeldTx(ctx, tx, ws, 1000, TypePatternMineHeld, "seed", nil); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, insertTrafficHoldSQL, reqID, ws, TypePatternMine, int64(1000), int64(time.Millisecond.Microseconds())); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	_ = tx.Commit(ctx)
}

// TestSinglePartyGuard_VelocityFarm_Flagged_HonestSettles is the Phase-4a Item 3
// RED-first proof: a per-workspace velocity spike on pattern held mints is flagged
// and does NOT settle (examined-but-flagged ⇒ never cleared ⇒ the fail-closed
// sweeper skips it); honest low-velocity workspaces settle normally.
func TestSinglePartyGuard_VelocityFarm_Flagged_HonestSettles(t *testing.T) {
	pool := singlePartyPool(t)
	ctx := context.Background()
	ledger := newLedgerStore(pool)

	// ws_farm: 8 held pattern mints in the window (a velocity spike, > threshold 5).
	for i := 0; i < 8; i++ {
		seedPatternHold(t, pool, ledger, fmt.Sprintf("farm-%d", i), "ws_farm")
	}
	// two honest workspaces: 2 each (below threshold).
	seedPatternHold(t, pool, ledger, "h1", "ws_hon1")
	seedPatternHold(t, pool, ledger, "h2", "ws_hon1")
	seedPatternHold(t, pool, ledger, "h3", "ws_hon2")

	// The detector flags the farm workspace, not the honest ones.
	det := NewSinglePartyConcentrationDetector(pool, TypePatternMine, 5, 24*time.Hour) // >5 held mints in 24h ⇒ spike
	examined, flagged, err := det.DetectAndPartition(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("DetectAndPartition: %v", err)
	}
	if len(examined) != 11 {
		t.Fatalf("examined=%d, want 11 (all held pattern rows)", len(examined))
	}
	flaggedWS := map[string]bool{}
	for _, k := range flagged {
		flaggedWS[k.WorkspaceID] = true
	}
	if !flaggedWS["ws_farm"] {
		t.Error("the velocity-farm workspace must be flagged")
	}
	if flaggedWS["ws_hon1"] || flaggedWS["ws_hon2"] {
		t.Error("honest low-velocity workspaces must NOT be flagged (no false positive)")
	}

	// The clearer promotes examined-clean-due held→cleared; the farm stays held.
	clearer := NewTrafficSettlementClearer(det, pool, func() bool { return true }, 24*time.Hour)
	if _, err := clearer.RunOnce(ctx); err != nil {
		t.Fatalf("clearer: %v", err)
	}
	time.Sleep(3 * time.Millisecond) // let finalize_after elapse

	// The fail-closed sweeper settles ONLY cleared rows.
	sw := NewTrafficMintSweeper(pool, ledger)
	sw.SetSettleStatus("cleared")
	if _, err := sw.RunOnce(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	// Honest settled (spendable); the farm's 8 mints are NOT spendable (withheld).
	var honBal, farmBal, farmHeld int64
	_ = pool.QueryRow(ctx, `SELECT COALESCE(balance,0) FROM lens_token_balances WHERE workspace_id='ws_hon1'`).Scan(&honBal)
	_ = pool.QueryRow(ctx, `SELECT COALESCE(balance,0), COALESCE(held_balance,0) FROM lens_token_balances WHERE workspace_id='ws_farm'`).Scan(&farmBal, &farmHeld)
	if honBal != 2000 {
		t.Errorf("honest wsB spendable=%d, want 2000 (settled)", honBal)
	}
	if farmBal != 0 || farmHeld != 8000 {
		t.Errorf("farm spendable=%d held=%d, want 0/8000 (flagged ⇒ withheld from settlement)", farmBal, farmHeld)
	}
}
