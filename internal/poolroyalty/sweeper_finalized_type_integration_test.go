package poolroyalty_test

// sweeper_finalized_type_integration_test.go — THE MISSING GUARD (real PG).
//
// The finalize sweeper is parameterized BY TABLE (it settles pool-royalty, distill,
// eval-contribution, routing-prediction, node-latency and confidential-compute claim
// rows with one kernel), but it used to hand FinalizeHeldTx a HARDCODED
// "pool royalty finalized" description AND take that call's hardcoded TypePoolRoyalty
// ledger type — so EVERY settled mint, whatever it was, landed in the ledger labelled
// pool_royalty. Amounts were exact and conservation was clean; only the LABEL lied.
// Per-mint-type supply/revenue — the closed-test calibration input — was therefore
// unusable: a workspace's pool_royalty total summed every mint type it had ever earned.
//
// Why no existing test caught it: routing_live_chain_integration_test.go and
// poi_joint_failclosed_integration_test.go both settle a NON-pool-royalty mint through
// this exact sweeper, but they assert minted_amount, balance/held_balance and status —
// never the settled ledger row's TYPE. The amounts were right, so they stayed green
// while the label was wrong. This file asserts the TYPE, for every mint that flows
// through the sweeper, so a future hardcode regression fails here.
//
// Two invariants, one per test:
//
//	(1) SettlesAsOwnMintType — the settled ledger row's type is the mint's OWN counted
//	    type, and the settled AMOUNT is exact. Amount and type are asserted TOGETHER so
//	    the RED run proves the bug is label-only: every amount assertion passes on main,
//	    only the type assertions fail.
//	(2) SupplyConserved — relabelling must not move a single µLENS. Every settled type
//	    must remain COUNTED in GetTotalSupply, so total supply after settling all six is
//	    byte-identical to the sum of the amounts minted. This is the guard against the
//	    obvious wrong fix: giving a mint its own final type but leaving that type out of
//	    GetTotalSupply's allow-list would silently drop it out of supply (and out of the
//	    LXC conversion math that reads it) — an attribution fix that changed the money.
//
// The held→final pairing is the design invariant these cases encode: a mint's counted
// final type is its held type minus the "_held" suffix (mining.heldTypeFor's convention),
// it is in GetTotalSupply's allow-list, and it is NOT in mintTypeList (finalize settles
// already-gated held value — it is not a new mint moment, so it must not be re-gated).

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/poolroyalty"
)

// finalTypeCase is ONE claim table the poolroyalty FinalizeSweeper settles. The set below
// is enumerated from the SIX poolroyalty.NewFinalizeSweeper call sites in cmd/lens/main.go
// (pool-royalty, distill, eval-contribution, routing-prediction, node-latency, confidential-
// compute); heldType is read from each minter's CreditHeldTx call — the mint moment that
// fixes what the settled row must be attributed to.
// wantFinal is pinned as a LITERAL, never as the constant the fix introduces: the ledger
// type is a PERSISTED wire value that rows already carry, so a test that read the constant
// would follow a rename and silently bless it. Pinning the literal forces any change to the
// settled label through a money-path review — the same discipline the routing live-chain
// test uses for the mint amount.
type finalTypeCase struct {
	table     string
	ws        string
	heldType  string // the type written at the MINT moment (in mintTypeList: gated + capped)
	wantFinal string // the counted type the SETTLED row must carry (literal, pinned)
	amount    int64  // µLENS — distinct per case so a mislabel is unambiguous
	note      string
}

// finalTypeCases enumerates every mint that flows through the poolroyalty finalize sweeper.
//
// pool-royalty and distill share a settled type by DESIGN, not by the bug: the distill
// minter deliberately reuses the Pool-B held kernel and mints TypePoolRoyaltyHeld
// (distill_minter.go), so pool_royalty IS its own correct settled type — its held row and
// its final row agree. Giving distill a distinct final type would break the held↔final
// pairing and would require changing its HELD type, i.e. the mint moment — a money-path
// change, not a label fix.
var finalTypeCases = []finalTypeCase{
	{
		table: "pool_royalty_mints", ws: "ws_pool",
		heldType: mining.TypePoolRoyaltyHeld, wantFinal: "pool_royalty",
		amount: 386, note: "cache pool royalty — the only mint that was ever correctly labelled",
	},
	{
		table: "distill_royalty_mints", ws: "ws_distill",
		heldType: mining.TypePoolRoyaltyHeld, wantFinal: "pool_royalty",
		amount: 1_000, note: "distill reuse royalty — mints pool_royalty_held by design, so pool_royalty is correct",
	},
	{
		table: "eval_contribution_mints", ws: "ws_eval",
		heldType: mining.TypeEvalContributionHeld, wantFinal: "eval_contribution",
		amount: 4_000, note: "P-o-I 1 — observed live settling as pool_royalty",
	},
	{
		table: "routing_prediction_mints", ws: "ws_routing",
		heldType: mining.TypeRoutingPredictionHeld, wantFinal: "eval_routing_prediction",
		amount: 10_000, note: "P-o-I 2 — observed live settling as pool_royalty",
	},
	{
		table: "node_latency_mints", ws: "ws_latency",
		heldType: mining.TypeLatencyLocalityHeld, wantFinal: "eval_latency_locality",
		amount: 7_000, note: "P-o-I 3 — the one live-reachable P-o-I mint",
	},
	{
		table: "confidential_compute_mints", ws: "ws_confidential",
		heldType: mining.TypeConfidentialComputeHeld, wantFinal: "eval_confidential_compute",
		amount: 9_000, note: "P-o-I 4 — inert by substrate absence, wired identically",
	},
}

// heldTypePairing pins the design invariant the cases above encode: each mint's counted
// final type is EXACTLY its held type minus the "_held" suffix (mining.heldTypeFor's
// convention, which the traffic mints already follow). A settled label that does not pair
// with its own held label is the bug this file exists to catch.
func TestSweeperFinalizedType_PairsWithHeldType(t *testing.T) {
	for _, c := range finalTypeCases {
		if got := c.wantFinal + "_held"; got != c.heldType {
			t.Errorf("%s: held/final pairing broken — final %q implies held %q, but the minter writes %q",
				c.table, c.wantFinal, got, c.heldType)
		}
	}
}

// finalTypePool builds the ledger + the given claim tables. Mirrors the schema the other
// real-PG sweeper tests build (jointPool): the generic finalize columns the kernel reads.
func finalTypePool(t *testing.T, tables ...string) (*pgxpool.Pool, *mining.LedgerStore) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping finalized-type integration test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ctx := context.Background()
	ddl := []string{
		// The margin views (migrations 0044 / distill PR4) depend on the royalty mint
		// tables, so a bare DROP TABLE fails with SQLSTATE 2BP01 against a fully-migrated
		// DB. Drop them first — the same convention linkage_integration_test.go uses; the
		// tests that need a margin view create it themselves.
		`DROP VIEW IF EXISTS pool_royalty_margin`,
		`DROP VIEW IF EXISTS distill_royalty_margin`,
		`DROP TABLE IF EXISTS ` + strings.Join(tables, ", ") + `, lens_token_balances, lens_token_ledger`,
		`CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY, balance BIGINT NOT NULL DEFAULT 0,
			held_balance BIGINT NOT NULL DEFAULT 0, lifetime_earned BIGINT NOT NULL DEFAULT 0,
			lifetime_spent BIGINT NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_ledger (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL,
			amount BIGINT NOT NULL, balance_after BIGINT NOT NULL, type TEXT NOT NULL,
			description TEXT, metadata JSONB, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
	}
	for _, tbl := range tables {
		ddl = append(ddl, `CREATE TABLE `+tbl+` (request_id TEXT PRIMARY KEY, contributor_workspace_id TEXT NOT NULL,
			minted_amount BIGINT NOT NULL, status TEXT NOT NULL DEFAULT 'held', finalize_after TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`)
	}
	for _, stmt := range ddl {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool, mining.NewLedgerStore(pool)
}

// seedHeldMint mints a held row through the REAL held kernel (CreditHeldTx with the mint's
// own held type) and claims the matching table row, already past finalize_after — exactly
// what each production minter commits.
func seedHeldMint(t *testing.T, pool *pgxpool.Pool, ledger *mining.LedgerStore, c finalTypeCase) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.CreditHeldTx(ctx, tx, c.ws, c.amount, c.heldType, "seed held mint", nil); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("%s: seed held credit: %v", c.table, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+c.table+`
		(request_id, contributor_workspace_id, minted_amount, status, finalize_after)
		VALUES ($1, $2, $3, 'held', now() - interval '1 second')`, "req-"+c.ws, c.ws, c.amount); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("%s: seed claim row: %v", c.table, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// settledRow reads the SETTLED (non-held, credit-side) ledger row for a workspace: the row
// the sweeper wrote at finalize. The held mint row is excluded by its _held suffix, so what
// is left is exactly the settlement row under test.
func settledRow(t *testing.T, pool *pgxpool.Pool, ws string) (rowType string, amount int64) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT type, amount FROM lens_token_ledger
		 WHERE workspace_id = $1 AND amount > 0 AND type NOT LIKE '%\_held'`, ws).Scan(&rowType, &amount); err != nil {
		t.Fatalf("read settled ledger row for %s: %v", ws, err)
	}
	return rowType, amount
}

// TestSweeperFinalizedType_SettlesAsOwnMintType is the guard that was missing: a mint settled
// by the table-parameterized sweeper must carry its OWN counted type in the ledger — not a
// hardcoded pool_royalty.
//
// RED on main for the four P-o-I tables: the settled row comes back type=pool_royalty. The
// amount assertion passes in the same run — proving the defect is attribution-only.
func TestSweeperFinalizedType_SettlesAsOwnMintType(t *testing.T) {
	for _, c := range finalTypeCases {
		t.Run(c.table, func(t *testing.T) {
			pool, ledger := finalTypePool(t, c.table)
			ctx := context.Background()
			seedHeldMint(t, pool, ledger, c)

			sw := poolroyalty.NewFinalizeSweeper(pool, ledger, c.table)
			n, err := sw.RunOnce(ctx)
			if err != nil {
				t.Fatalf("%s: finalize sweep: %v", c.table, err)
			}
			if n != 1 {
				t.Fatalf("%s: finalized %d rows, want 1", c.table, n)
			}

			gotType, gotAmount := settledRow(t, pool, c.ws)

			// THE LABEL — the assertion no existing test made.
			if gotType != c.wantFinal {
				t.Errorf("%s: settled ledger row type = %q, want %q\n"+
					"  the sweeper is parameterized BY TABLE but labelled the settled row with a hardcoded type;\n"+
					"  %s mints must settle as their own counted type or per-mint-type calibration is corrupted\n"+
					"  (%s)", c.table, gotType, c.wantFinal, c.table, c.note)
			}
			// THE MONEY — must be untouched by the label fix, before AND after.
			if gotAmount != c.amount {
				t.Errorf("%s: settled amount = %d µLENS, want %d — the fix must move the LABEL, never a µLENS",
					c.table, gotAmount, c.amount)
			}
			// held → spendable moved exactly, nothing stranded.
			var bal, held int64
			_ = pool.QueryRow(ctx, `SELECT COALESCE(balance,0), COALESCE(held_balance,0)
				FROM lens_token_balances WHERE workspace_id=$1`, c.ws).Scan(&bal, &held)
			if bal != c.amount || held != 0 {
				t.Errorf("%s: spendable=%d held=%d, want %d/0 (settlement conserves owned value)",
					c.table, bal, held, c.amount)
			}
		})
	}
}

// TestSweeperFinalizedType_SupplyConserved pins the invariant that makes this an attribution
// fix and not a money change: EVERY settled type stays counted in GetTotalSupply, so total
// supply is identical whether the rows are labelled honestly or all lumped as pool_royalty.
//
// This fails loudly on the tempting wrong fix — minting a new final type without adding it to
// GetTotalSupply's allow-list — which would drop those µLENS out of supply and out of the LXC
// conversion math that reads it.
func TestSweeperFinalizedType_SupplyConserved(t *testing.T) {
	tables := make([]string, 0, len(finalTypeCases))
	for _, c := range finalTypeCases {
		tables = append(tables, c.table)
	}
	pool, ledger := finalTypePool(t, tables...)
	ctx := context.Background()

	var wantSupply int64
	for _, c := range finalTypeCases {
		seedHeldMint(t, pool, ledger, c)
		wantSupply += c.amount
	}

	// Held mints have not entered circulation yet: supply must still be zero.
	if s, err := ledger.GetTotalSupply(ctx); err != nil {
		t.Fatal(err)
	} else if s != 0 {
		t.Fatalf("supply before settlement = %d, want 0 (a held mint is uncounted until it settles)", s)
	}

	for _, c := range finalTypeCases {
		sw := poolroyalty.NewFinalizeSweeper(pool, ledger, c.table)
		if _, err := sw.RunOnce(ctx); err != nil {
			t.Fatalf("%s: finalize sweep: %v", c.table, err)
		}
	}

	gotSupply, err := ledger.GetTotalSupply(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gotSupply != wantSupply {
		t.Errorf("total supply after settling every mint = %d µLENS, want %d\n"+
			"  every settled type MUST stay in GetTotalSupply's allow-list: attributing a mint to its own\n"+
			"  type must not drop it out of supply — that would change the money, not just the label",
			gotSupply, wantSupply)
	}

	// And the attribution itself: per-type sums must now reflect what was actually minted.
	// This is the calibration input the bug corrupted — pool_royalty read as the sum of everything.
	for _, want := range []struct {
		typ string
		sum int64
	}{
		{"pool_royalty", 386 + 1_000}, // cache royalty + distill (share a settled type by design)
		{"eval_contribution", 4_000},
		{"eval_routing_prediction", 10_000},
		{"eval_latency_locality", 7_000},
		{"eval_confidential_compute", 9_000},
	} {
		var got int64
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(SUM(amount),0) FROM lens_token_ledger WHERE amount > 0 AND type = $1`, want.typ).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want.sum {
			t.Errorf("per-type settled supply for %q = %d µLENS, want %d (per-mint-type calibration input)",
				want.typ, got, want.sum)
		}
	}
}
