package poolroyalty

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/mining"
)

// singlePartyPoolPOI builds the ledger + one single-party P-o-I mint table (the
// generic finalize columns) for the examination-gate test.
func singlePartyPoolPOI(t *testing.T, table string) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG single-party P-o-I test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS ` + table + `, lens_token_balances, lens_token_ledger`,
		`CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY, balance BIGINT NOT NULL DEFAULT 0,
			held_balance BIGINT NOT NULL DEFAULT 0, lifetime_earned BIGINT NOT NULL DEFAULT 0,
			lifetime_spent BIGINT NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_ledger (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL,
			amount BIGINT NOT NULL, balance_after BIGINT NOT NULL, type TEXT NOT NULL,
			description TEXT, metadata JSONB, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE ` + table + ` (request_id TEXT PRIMARY KEY, contributor_workspace_id TEXT NOT NULL,
			minted_amount BIGINT NOT NULL, status TEXT NOT NULL DEFAULT 'held', finalize_after TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool
}

// seedPOIHeld writes a held single-party P-o-I mint (ledger held credit + claim row).
func seedPOIHeld(t *testing.T, pool *pgxpool.Pool, ledger *mining.LedgerStore, table, reqID, ws string) {
	t.Helper()
	ctx := context.Background()
	tx, _ := pool.Begin(ctx)
	if err := ledger.CreditHeldTx(ctx, tx, ws, 1000, mining.TypeRoutingPredictionHeld, "seed", nil); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+table+` (request_id, contributor_workspace_id, minted_amount, status, finalize_after)
		VALUES ($1,$2,1000,'held', now()+interval '1 millisecond')`, reqID, ws); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	_ = tx.Commit(ctx)
}

// TestSinglePartyPOI_ExaminedBeforeSettle is the P-o-I examination-gate proof: a
// single-party velocity farm on routing_prediction_mints is flagged → never
// cleared → NOT settled by the fail-closed FinalizeSweeper (so it does NOT strand
// silently — it's examined and withheld); an honest low-velocity workspace clears
// and settles. This is the gate BOTH new P-o-I mints ride so nothing strands.
func TestSinglePartyPOI_ExaminedBeforeSettle(t *testing.T) {
	const table = "routing_prediction_mints"
	pool := singlePartyPoolPOI(t, table)
	ctx := context.Background()
	ledger := mining.NewLedgerStore(pool)

	for i := 0; i < 8; i++ { // ws_farm velocity spike
		seedPOIHeld(t, pool, ledger, table, fmt.Sprintf("poi-farm-%d", i), "ws_farm")
	}
	seedPOIHeld(t, pool, ledger, table, "poi-h1", "ws_hon")
	seedPOIHeld(t, pool, ledger, table, "poi-h2", "ws_hon")

	det := NewSinglePartyConcentrationDetector(pool, table, 5, 24*time.Hour)
	examined, flags, err := det.DetectAndPartition(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("DetectAndPartition: %v", err)
	}
	if len(examined) != 10 {
		t.Fatalf("examined=%d, want 10 (every held row is examined)", len(examined))
	}
	flaggedReq := map[string]bool{}
	for _, f := range flags {
		flaggedReq[f.RequestID] = true
	}
	if !flaggedReq["poi-farm-0"] || flaggedReq["poi-h1"] {
		t.Fatalf("velocity farm must be flagged, honest not (flagged=%v)", flaggedReq)
	}

	// The Phase-3 SettlementClearer + fail-closed FinalizeSweeper handle it.
	clearer := NewSettlementClearer(det, pool, table, func() bool { return true }, 24*time.Hour)
	if _, err := clearer.RunOnce(ctx); err != nil {
		t.Fatalf("clearer: %v", err)
	}
	time.Sleep(4 * time.Millisecond)
	sw := NewFinalizeSweeper(pool, ledger, table)
	sw.SetSettleStatus("cleared")
	if _, err := sw.RunOnce(ctx); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	var honBal, farmBal, farmHeld int64
	_ = pool.QueryRow(ctx, `SELECT COALESCE(balance,0) FROM lens_token_balances WHERE workspace_id='ws_hon'`).Scan(&honBal)
	_ = pool.QueryRow(ctx, `SELECT COALESCE(balance,0), COALESCE(held_balance,0) FROM lens_token_balances WHERE workspace_id='ws_farm'`).Scan(&farmBal, &farmHeld)
	if honBal != 2000 {
		t.Errorf("honest settled=%d, want 2000", honBal)
	}
	if farmBal != 0 || farmHeld != 8000 {
		t.Errorf("farm spendable=%d held=%d, want 0/8000 (examined→flagged→withheld, NOT stranded silently)", farmBal, farmHeld)
	}
}
