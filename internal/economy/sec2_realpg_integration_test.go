package economy

// SEC-2 real-Postgres proof. pgxmock returns whatever the test feeds it, so it
// CANNOT catch a BIGINT/DOUBLE type mismatch between the migrated schema and the
// int64 Go scans. This test applies the ACTUAL migration set (0001…0082, via the
// real dbmigrate runner over the embedded migrations.FS) to a throwaway Postgres,
// then round-trips a LENS credit→debit→balance and an LXC credit→spend→balance
// through the REAL LedgerStore / DualTokenStore, asserting the integer µ-values
// land in BIGINT columns and come back EXACTLY. It also asserts the physical
// column types are BIGINT (belt-and-suspenders) and that the partitioned-table
// ALTERs in 0081/0082 actually applied.
//
//	LENS_TEST_DATABASE_URL=postgres://…  go test ./internal/economy/ -run TestSEC2_RealPG -v

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/migrations"
)

func TestSEC2_RealPG_LedgerRoundTripThroughMigratedSchema(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping SEC-2 real-PG proof")
	}
	ctx := context.Background()

	// Isolate into a PRIVATE schema (the economy TestMain pins search_path to
	// lens_it_economy, which the other economy integration tests DROP/CREATE
	// tables in — applying the FULL migration set there would collide under the
	// shared-DB `go test ./...` run). Pinning our own schema keeps this genuinely
	// disjoint (mirrors povi/stakes_concurrency_integration_test.go).
	const schema = "sec2_realpg_proof"

	// ── apply the REAL migration set (incl. 0081/0082) via the real runner ──
	connCfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse conn config: %v", err)
	}
	connCfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, connCfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	for _, ddl := range []string{`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`, `CREATE SCHEMA ` + schema} {
		if _, err := conn.Exec(ctx, ddl); err != nil {
			t.Fatalf("reset schema: %v", err)
		}
	}
	applied, err := dbmigrate.Run(ctx, conn, migrations.FS)
	if err != nil {
		t.Fatalf("apply real migrations (0001…0082): %v", err)
	}
	t.Logf("applied %d migrations into schema %q; last = %s", len(applied), schema, applied[len(applied)-1])
	_ = conn.Close(ctx)

	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// (0) the physical column types must be BIGINT after 0081/0082/0083 — the whole
	// point. This is the AUTHORITATIVE divergence-catcher: it reads the REAL migrated
	// schema, so ANY conserved money column shipping DOUBLE (a new mint table born
	// float, a reverted ALTER) goes RED here — the systemic backstop the per-file test
	// DDL duplication otherwise lacks. Phase-4-PRE: extended to EVERY held-mint table's
	// minted_amount (was only pool_royalty_mints) + the balances/ledger/stake/lxc set.
	for _, c := range []struct{ table, col string }{
		{"lens_token_ledger", "amount"}, {"lens_token_ledger", "balance_after"},
		{"lens_token_balances", "balance"}, {"lens_token_balances", "held_balance"},
		{"lens_token_balances", "locked_balance"},
		{"lens_token_balances", "lifetime_earned"}, {"lens_token_balances", "lifetime_spent"},
		{"lxc_balances", "balance"}, {"lxc_ledger", "amount"}, {"lxc_ledger", "balance_after"},
		{"lxc_purchases", "lxc_amount"}, {"lxc_spend_claims", "lxc_amount"},
		{"agent_lxc_subbudgets", "ceiling_lxc"}, {"agent_lxc_subbudgets", "spent_lxc"},
		{"annotator_stakes", "staked"}, {"stake_positions", "amount"},
		{"marketplace_trades", "talyvor_fee"}, {"marketplace_trades", "amount"},
		{"povi_stakes", "amount"}, {"povi_stakes", "slashed_amount"},
		{"routing_patterns", "earned"}, {"pattern_mine_credits", "earned"},
		{"mint_idempotency", "amount"}, {"provenance_bonds", "amount_ulens"},
		// EVERY held-mint minted_amount — the crux of this investigation. All eight
		// must be BIGINT µLENS (six via 0082:84-89, traffic born BIGINT in 0090).
		{"pool_royalty_mints", "minted_amount"}, {"distill_royalty_mints", "minted_amount"},
		{"eval_contribution_mints", "minted_amount"}, {"routing_prediction_mints", "minted_amount"},
		{"node_latency_mints", "minted_amount"}, {"confidential_compute_mints", "minted_amount"},
		{"traffic_mint_holds", "minted_amount"},
	} {
		var typ string
		if err := pool.QueryRow(ctx,
			`SELECT data_type FROM information_schema.columns WHERE table_schema=$1 AND table_name=$2 AND column_name=$3`,
			schema, c.table, c.col).Scan(&typ); err != nil {
			t.Fatalf("type of %s.%s: %v", c.table, c.col, err)
		}
		if typ != "bigint" {
			t.Errorf("%s.%s is %q, want bigint (a conserved LENS/LXC column must be integer µ-units — SEC-2)", c.table, c.col, typ)
		}
	}

	// (0b) The DISTINGUISHER: the Tier-3 USD/score columns that SEC-2 deliberately
	// KEPT as DOUBLE must still be DOUBLE. Pinning both directions proves this guard
	// genuinely reads the type (it would catch a conserved col drifting to DOUBLE AND
	// a USD col being wrongly converted to BIGINT) — it is not coercing them together.
	for _, c := range []struct{ table, col string }{
		{"pool_royalty_mints", "avoided_cogs_usd"}, {"distill_royalty_mints", "avoided_cogs_usd"},
		{"routing_prediction_mints", "skill_margin"}, {"inference_nodes", "price_per_token"},
	} {
		var typ string
		if err := pool.QueryRow(ctx,
			`SELECT data_type FROM information_schema.columns WHERE table_schema=$1 AND table_name=$2 AND column_name=$3`,
			schema, c.table, c.col).Scan(&typ); err != nil {
			t.Fatalf("type of %s.%s: %v", c.table, c.col, err)
		}
		if typ != "double precision" {
			t.Errorf("%s.%s is %q, want double precision (a Tier-2/3 USD/rate/score column — SEC-2 keeps these float)", c.table, c.col, typ)
		}
	}

	// No workspaces row needed: the money tables have no FK to workspaces and the
	// ledger auto-upserts the balance row (no mint verifier is wired here, so the
	// U6 earn-gate is a no-op).
	const ws = "ws_sec2_realpg"

	// ── LENS: credit 1.5 LENS, debit 0.5 LENS → balance EXACTLY 1.0 LENS (µLENS) ──
	ledger := mining.NewLedgerStore(pool)
	const credit, debit int64 = 1_500_000, 500_000 // µLENS
	if err := ledger.Credit(ctx, ws, credit, mining.TypeCacheMine, "sec2 realpg credit", nil); err != nil {
		t.Fatalf("Credit: %v", err)
	}
	if err := ledger.Debit(ctx, ws, debit, mining.TypeSpend, "sec2 realpg debit", nil); err != nil {
		t.Fatalf("Debit: %v", err)
	}
	bal, err := ledger.GetBalance(ctx, ws)
	if err != nil {
		t.Fatalf("GetBalance: %v", err)
	}
	if bal != credit-debit { // 1_000_000 µLENS, exact
		t.Fatalf("LENS balance = %d µLENS, want %d (exact int64 round-trip through BIGINT)", bal, credit-debit)
	}
	snap, err := ledger.GetSnapshot(ctx, ws)
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if snap.Balance != 1_000_000 || snap.LifetimeEarned != credit || snap.LifetimeSpent != debit {
		t.Fatalf("snapshot = %+v, want balance=1_000_000 earned=%d spent=%d", snap, credit, debit)
	}
	// the append-only ledger's balance_after landed as an exact BIGINT.
	var lastBalanceAfter int64
	if err := pool.QueryRow(ctx,
		`SELECT balance_after FROM lens_token_ledger WHERE workspace_id=$1 ORDER BY created_at DESC, amount ASC LIMIT 1`,
		ws).Scan(&lastBalanceAfter); err != nil {
		t.Fatalf("read ledger balance_after: %v", err)
	}
	if lastBalanceAfter != 1_000_000 {
		t.Fatalf("ledger balance_after = %d µLENS, want 1_000_000", lastBalanceAfter)
	}

	// ── LXC: credit 2.5 LXC, spend 0.5 LXC → balance EXACTLY 2.0 LXC (µLXC) ──
	dt := NewDualTokenStore(ledger, pool, NewRateEngine(ledger, pool))
	const lxcCredit, lxcSpend int64 = 2_500_000, 500_000 // µLXC
	if _, err := dt.CreditLXC(ctx, ws, lxcCredit, "sec2 realpg lxc credit", nil); err != nil {
		t.Fatalf("CreditLXC: %v", err)
	}
	if err := dt.SpendLXC(ctx, ws, lxcSpend, "sec2 realpg lxc spend"); err != nil {
		t.Fatalf("SpendLXC: %v", err)
	}
	lxcBal, err := dt.GetLXCBalance(ctx, ws)
	if err != nil {
		t.Fatalf("GetLXCBalance: %v", err)
	}
	if lxcBal != lxcCredit-lxcSpend { // 2_000_000 µLXC, exact
		t.Fatalf("LXC balance = %d µLXC, want %d (exact int64 round-trip through BIGINT)", lxcBal, lxcCredit-lxcSpend)
	}
	// USD-value display (site #6): floor(2_000_000 µLXC × $0.10) = 200_000 µUSD ($0.20).
	lxcSnap, err := dt.GetLXCSnapshot(ctx, ws)
	if err != nil {
		t.Fatalf("GetLXCSnapshot: %v", err)
	}
	if lxcSnap.Balance != 2_000_000 || lxcSnap.USDValue != mining.MulFloor(2_000_000, LXCUSDValue) {
		t.Fatalf("lxc snapshot = %+v, want balance=2_000_000 usd=%d µUSD", lxcSnap, mining.MulFloor(2_000_000, LXCUSDValue))
	}
}
