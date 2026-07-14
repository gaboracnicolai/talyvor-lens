package poolroyalty_test

// poi_joint_failclosed_integration_test.go — the JOINT acceptance proof for BOTH new P-o-I mints: routing-
// prediction and eval-contribution held rows ride the SAME armed fail-closed settlement guard. For EACH
// table it proves the two-sided invariant:
//   (1) FAIL-CLOSED: the armed FinalizeSweeper (settle status 'cleared') alone settles NOTHING — an
//       un-examined held row is WITHHELD, not stranded silently (it stays held, recoverable), and
//   (2) EXAMINED-THEN-SETTLE: after the single-party detector + clearer examine and clear it, the honest
//       held row settles.
// Together: neither new mint can settle without examination, and neither strands once examined.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/poolroyalty"
)

func jointPool(t *testing.T, table string) (*pgxpool.Pool, *mining.LedgerStore) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping joint fail-closed test")
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
	return pool, mining.NewLedgerStore(pool)
}

func seedHeld(t *testing.T, pool *pgxpool.Pool, ledger *mining.LedgerStore, table, reqID, ws string, mintType string) {
	t.Helper()
	ctx := context.Background()
	tx, _ := pool.Begin(ctx)
	if err := ledger.CreditHeldTx(ctx, tx, ws, 2000, mintType, "seed", nil); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+table+` (request_id, contributor_workspace_id, minted_amount, status, finalize_after)
		VALUES ($1,$2,2000,'held', now()-interval '1 second')`, reqID, ws); err != nil { // already past finalize
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	_ = tx.Commit(ctx)
}

func heldSpendable(t *testing.T, pool *pgxpool.Pool, ws string) (bal, held int64) {
	t.Helper()
	_ = pool.QueryRow(context.Background(),
		`SELECT COALESCE(balance,0), COALESCE(held_balance,0) FROM lens_token_balances WHERE workspace_id=$1`, ws).Scan(&bal, &held)
	return
}

func TestPOIJoint_BothMintsFailClosedAndNeitherStrands(t *testing.T) {
	cases := []struct {
		table    string
		mintType string
	}{
		{"routing_prediction_mints", mining.TypeRoutingPredictionHeld},
		{"eval_contribution_mints", mining.TypeEvalContributionHeld},
	}
	for _, c := range cases {
		t.Run(c.table, func(t *testing.T) {
			pool, ledger := jointPool(t, c.table)
			ctx := context.Background()
			seedHeld(t, pool, ledger, c.table, "honest-1", "ws_hon", c.mintType)

			// (1) FAIL-CLOSED: the armed sweeper alone (nothing examined→cleared) settles NOTHING.
			sw := poolroyalty.NewFinalizeSweeper(pool, ledger, c.table)
			sw.SetSettleStatus("cleared") // armed: only 'cleared' rows settle
			if _, err := sw.RunOnce(ctx); err != nil {
				t.Fatalf("armed finalize: %v", err)
			}
			bal, held := heldSpendable(t, pool, "ws_hon")
			if bal != 0 || held != 2000 {
				t.Fatalf("%s FAIL-CLOSED: un-examined held row must NOT settle (spendable=%d held=%d, want 0/2000 — withheld, not stranded)", c.table, bal, held)
			}

			// (2) EXAMINED-THEN-SETTLE: the detector + clearer examine + clear it; the armed sweeper then settles.
			det := poolroyalty.NewSinglePartyConcentrationDetector(pool, c.table, 100, 24*time.Hour)
			clearer := poolroyalty.NewSettlementClearer(det, pool, c.table, func() bool { return true }, 24*time.Hour)
			if _, err := clearer.RunOnce(ctx); err != nil {
				t.Fatalf("clearer: %v", err)
			}
			if _, err := sw.RunOnce(ctx); err != nil {
				t.Fatalf("finalize after clear: %v", err)
			}
			bal, held = heldSpendable(t, pool, "ws_hon")
			if bal != 2000 || held != 0 {
				t.Fatalf("%s EXAMINED: honest row must settle after examine+clear (spendable=%d held=%d, want 2000/0)", c.table, bal, held)
			}
		})
	}
}
