package poolroyalty_test

// revoker_revoked_type_integration_test.go — THE MISSING GUARD for the CLAWBACK label (real PG).
//
// The poolroyalty.Revoker is parameterized BY TABLE (NewRevokerForTable claws back pool-royalty,
// distill, eval-contribution, routing-prediction, node-latency and confidential-compute held
// mints with one kernel), but it takes the type-LESS mining.RevokeHeldTx — whose hardcoded
// TypePoolRoyaltyRevoked makes EVERY clawback, whatever mint it reverses, land in the ledger
// labelled pool_royalty_revoked. This is the exact sibling of the #312 finalize bug, on the
// revoke side: the sweeper's settled label was fixed, the revoker's was not.
//
// Why no existing test caught it: held_clawback_realschema_integration_test.go revokes a mint in
// EACH P-o-I family table through this exact Revoker, but asserts outcome, status and the burned
// HELD BALANCE — never the revoke ledger row's TYPE. The amounts/balances were right, so it
// stayed green while the label lied. This file asserts the TYPE, for every mint revoked through
// the Revoker, so a future hardcode regression fails here. (The #312 lesson, restated: an
// amount-only test passes while the label is wrong.)
//
// The money-safety INVERSION vs #312: a SETTLED (finalize) type MUST be counted in
// GetTotalSupply (settlement is when a mint enters supply). A REVOKE type must be counted in
// NEITHER GetTotalSupply NOR GetTotalBurned: the held LENS never entered supply, so reversing it
// must move total supply by nothing and must not register as a burn. SupplyAndBurnUnchanged pins
// exactly that — the guard against the tempting wrong fix (adding a *_revoked type to either
// total's allow-list would turn a relabel into a money change, the mirror of the finalize trap).

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/poolroyalty"
)

// revokeTypeCase is ONE claim table the poolroyalty Revoker claws back. The set below is
// enumerated from the SIX poolroyalty.NewRevoker / NewRevokerForTable call sites in
// cmd/lens/main.go (pool-royalty, distill, eval-contribution, routing-prediction, node-latency,
// confidential-compute); heldType is read from each minter's CreditHeldTx call — the mint moment
// that fixes what the clawback row must be attributed to.
// wantRevoke is pinned as a LITERAL, never as the constant the fix introduces: the ledger type is
// a PERSISTED wire value that rows already carry, so a test that read the constant would follow a
// rename and silently bless it. Pinning the literal forces any change to the clawback label
// through a money-path review — the same discipline the finalize type test uses.
type revokeTypeCase struct {
	table      string
	ws         string
	heldType   string // the type written at the MINT moment (in mintTypeList: gated + capped)
	wantRevoke string // the SUPPLY-NEUTRAL type the CLAWBACK row must carry (literal, pinned)
	amount     int64  // µLENS — distinct per case so a mislabel is unambiguous
	note       string
}

// revokeTypeCases enumerates every mint that flows through the poolroyalty Revoker.
//
// pool-royalty and distill share a clawback type by DESIGN, not by the bug: the distill minter
// deliberately reuses the Pool-B held kernel and mints TypePoolRoyaltyHeld, so pool_royalty_revoked
// IS its own correct clawback type — its held row and its revoke row agree. Giving distill a
// distinct revoke type would break the held↔revoke pairing and would require changing its HELD
// type, i.e. the mint moment — a money-path change, not a label fix. (Same reasoning #312 used to
// keep distill's finalize type pool_royalty.)
var revokeTypeCases = []revokeTypeCase{
	{
		table: "pool_royalty_mints", ws: "ws_pool",
		heldType: mining.TypePoolRoyaltyHeld, wantRevoke: "pool_royalty_revoked",
		amount: 386, note: "cache pool royalty — the only clawback that was ever correctly labelled",
	},
	{
		table: "distill_royalty_mints", ws: "ws_distill",
		heldType: mining.TypePoolRoyaltyHeld, wantRevoke: "pool_royalty_revoked",
		amount: 1_000, note: "distill reuse royalty — mints pool_royalty_held by design, so pool_royalty_revoked is correct",
	},
	{
		table: "eval_contribution_mints", ws: "ws_eval",
		heldType: mining.TypeEvalContributionHeld, wantRevoke: "eval_contribution_revoked",
		amount: 4_000, note: "P-o-I 1 — clawback mislabelled pool_royalty_revoked on main",
	},
	{
		table: "routing_prediction_mints", ws: "ws_routing",
		heldType: mining.TypeRoutingPredictionHeld, wantRevoke: "eval_routing_prediction_revoked",
		amount: 10_000, note: "P-o-I 2 — clawback mislabelled pool_royalty_revoked on main",
	},
	{
		table: "node_latency_mints", ws: "ws_latency",
		heldType: mining.TypeLatencyLocalityHeld, wantRevoke: "eval_latency_locality_revoked",
		amount: 7_000, note: "P-o-I 3 — the one live-reachable P-o-I mint",
	},
	{
		table: "confidential_compute_mints", ws: "ws_confidential",
		heldType: mining.TypeConfidentialComputeHeld, wantRevoke: "eval_confidential_compute_revoked",
		amount: 9_000, note: "P-o-I 4 — inert by substrate absence, wired identically",
	},
}

// TestRevokerRevokedType_PairsWithHeldType pins the design invariant the cases encode: each mint's
// clawback type is EXACTLY its held type minus the "_held" suffix, plus "_revoked" (the held↔final
// convention #312 introduced, extended one hop to the revoke). A clawback label that does not pair
// with its own held label is the bug this file exists to catch.
func TestRevokerRevokedType_PairsWithHeldType(t *testing.T) {
	for _, c := range revokeTypeCases {
		if base := strings.TrimSuffix(c.heldType, "_held"); base+"_revoked" != c.wantRevoke {
			t.Errorf("%s: held/revoke pairing broken — held %q implies revoke %q, but the case pins %q",
				c.table, c.heldType, base+"_revoked", c.wantRevoke)
		}
	}
}

// revokeTypePool builds the ledger + the given claim tables in a DEDICATED schema (isolated from
// the finalize-type test's public-schema tables, and from a fully-migrated DB). minted_amount is
// BIGINT µLENS, matching the real migration (0082) — the same production column type the
// held-clawback real-schema harness pins.
func revokeTypePool(t *testing.T, tables ...string) (*pgxpool.Pool, *mining.LedgerStore) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping revoked-type integration test")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "lens_revoketype_test"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ctx := context.Background()
	ddl := []string{
		`DROP SCHEMA IF EXISTS lens_revoketype_test CASCADE`,
		`CREATE SCHEMA lens_revoketype_test`,
		`CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY, balance BIGINT NOT NULL DEFAULT 0,
			held_balance BIGINT NOT NULL DEFAULT 0, lifetime_earned BIGINT NOT NULL DEFAULT 0,
			lifetime_spent BIGINT NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_ledger (id UUID NOT NULL DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL,
			amount BIGINT NOT NULL, balance_after BIGINT NOT NULL, type TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '', metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (id, workspace_id))`,
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

// seedHeldMintForRevoke mints a held row through the REAL held kernel (CreditHeldTx with the mint's
// own held type) and claims the matching table row as 'held' — the exact state a production minter
// commits, and the only state the Revoker's CAS will transition.
func seedHeldMintForRevoke(t *testing.T, pool *pgxpool.Pool, ledger *mining.LedgerStore, c revokeTypeCase) {
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
		VALUES ($1, $2, $3, 'held', now() + interval '1 hour')`, "req-"+c.ws, c.ws, c.amount); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("%s: seed claim row: %v", c.table, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// revokeRow reads the CLAWBACK ledger row for a workspace: the row the Revoker wrote at revoke.
// The held mint row is +amount; the clawback is the sole NEGATIVE (debit-of-held) row, so amount<0
// selects exactly the row under test.
func revokeRow(t *testing.T, pool *pgxpool.Pool, ws string) (rowType string, amount int64) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT type, amount FROM lens_token_ledger WHERE workspace_id = $1 AND amount < 0`, ws).Scan(&rowType, &amount); err != nil {
		t.Fatalf("read clawback ledger row for %s: %v", ws, err)
	}
	return rowType, amount
}

// TestRevokerRevokedType_RecordsOwnRevokeType is the guard that was missing: a held mint clawed
// back by the table-parameterized Revoker must carry its OWN supply-neutral clawback type in the
// ledger — not a hardcoded pool_royalty_revoked.
//
// RED on main for the four P-o-I tables: the clawback row comes back type=pool_royalty_revoked.
// The amount + balance assertions pass in the same run — proving the defect is attribution-only.
func TestRevokerRevokedType_RecordsOwnRevokeType(t *testing.T) {
	for _, c := range revokeTypeCases {
		t.Run(c.table, func(t *testing.T) {
			pool, ledger := revokeTypePool(t, c.table)
			ctx := context.Background()
			seedHeldMintForRevoke(t, pool, ledger, c)

			rev := poolroyalty.NewRevokerForTable(pool, ledger, c.table)
			rep, _ := rev.RevokeHeldMints(ctx, []string{"req-" + c.ws})
			if rep.Outcomes["req-"+c.ws] != poolroyalty.OutcomeRevoked {
				t.Fatalf("%s: revoke outcome %v, want revoked", c.table, rep.Outcomes["req-"+c.ws])
			}

			gotType, gotAmount := revokeRow(t, pool, c.ws)

			// THE LABEL — the assertion no existing test made.
			if gotType != c.wantRevoke {
				t.Errorf("%s: clawback ledger row type = %q, want %q\n"+
					"  the Revoker is parameterized BY TABLE but labelled the clawback row with a hardcoded type;\n"+
					"  %s clawbacks must record their own supply-neutral type or per-mint-type accounting is corrupted\n"+
					"  (%s)", c.table, gotType, c.wantRevoke, c.table, c.note)
			}
			// THE MONEY — must be untouched by the label fix, before AND after. The clawback debits
			// exactly the held amount (recorded as a negative ledger row).
			if gotAmount != -c.amount {
				t.Errorf("%s: clawback amount = %d µLENS, want %d — the fix must move the LABEL, never a µLENS",
					c.table, gotAmount, -c.amount)
			}
			// held burned to 0, spendable never touched (a revoke removes held value only).
			var bal, held int64
			_ = pool.QueryRow(ctx, `SELECT COALESCE(balance,0), COALESCE(held_balance,0)
				FROM lens_token_balances WHERE workspace_id=$1`, c.ws).Scan(&bal, &held)
			if bal != 0 || held != 0 {
				t.Errorf("%s: spendable=%d held=%d, want 0/0 (a clawback burns held, never touches spendable)",
					c.table, bal, held)
			}
		})
	}
}

// TestRevokerRevokedType_SupplyAndBurnUnchanged pins the invariant that makes this an attribution
// fix and NOT a money change — the mirror-image of #312's SupplyConserved. A finalize type must be
// COUNTED; a revoke type must be counted by NEITHER total. Clawing back every held mint must leave
// total supply AND total burned at zero (the held LENS never entered supply, so reversing it is
// supply-neutral and is not a burn).
//
// This fails loudly on the tempting wrong fix — giving a clawback its own type but adding that type
// to GetTotalSupply's or GetTotalBurned's allow-list — which would make a relabel move money.
func TestRevokerRevokedType_SupplyAndBurnUnchanged(t *testing.T) {
	tables := make([]string, 0, len(revokeTypeCases))
	for _, c := range revokeTypeCases {
		tables = append(tables, c.table)
	}
	pool, ledger := revokeTypePool(t, tables...)
	ctx := context.Background()

	for _, c := range revokeTypeCases {
		seedHeldMintForRevoke(t, pool, ledger, c)
	}
	// Held mints have not entered circulation: supply and burned are both zero before any revoke.
	assertSupplyBurn(t, ledger, "after seeding held mints", 0, 0)

	for _, c := range revokeTypeCases {
		rev := poolroyalty.NewRevokerForTable(pool, ledger, c.table)
		rep, _ := rev.RevokeHeldMints(ctx, []string{"req-" + c.ws})
		if rep.Outcomes["req-"+c.ws] != poolroyalty.OutcomeRevoked {
			t.Fatalf("%s: revoke outcome %v, want revoked", c.table, rep.Outcomes["req-"+c.ws])
		}
	}
	// Clawing back held mints removes held value only: total supply stays 0 (nothing ever settled)
	// and total burned stays 0 (a revoke of held is NOT a burn — its type is in neither allow-list).
	assertSupplyBurn(t, ledger, "after clawing back every held mint", 0, 0)

	// And the attribution itself: per-type clawback sums must reflect what each mint actually was.
	// On main every clawback is lumped as pool_royalty_revoked, so these per-type sums are wrong.
	for _, want := range []struct {
		typ string
		sum int64 // negative: clawbacks are debit-of-held rows
	}{
		{"pool_royalty_revoked", -(386 + 1_000)}, // cache royalty + distill (share a clawback type by design)
		{"eval_contribution_revoked", -4_000},
		{"eval_routing_prediction_revoked", -10_000},
		{"eval_latency_locality_revoked", -7_000},
		{"eval_confidential_compute_revoked", -9_000},
	} {
		var got int64
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(SUM(amount),0) FROM lens_token_ledger WHERE amount < 0 AND type = $1`, want.typ).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want.sum {
			t.Errorf("per-type clawback sum for %q = %d µLENS, want %d (per-mint-type accounting)",
				want.typ, got, want.sum)
		}
	}
}

// assertSupplyBurn checks total supply and total burned together — the two money aggregates a
// clawback must leave untouched.
func assertSupplyBurn(t *testing.T, ledger *mining.LedgerStore, when string, wantSupply, wantBurned int64) {
	t.Helper()
	ctx := context.Background()
	supply, err := ledger.GetTotalSupply(ctx)
	if err != nil {
		t.Fatalf("GetTotalSupply %s: %v", when, err)
	}
	if supply != wantSupply {
		t.Errorf("total supply %s = %d µLENS, want %d — a held-mint clawback must not move supply", when, supply, wantSupply)
	}
	burned, err := ledger.GetTotalBurned(ctx)
	if err != nil {
		t.Fatalf("GetTotalBurned %s: %v", when, err)
	}
	if burned != wantBurned {
		t.Errorf("total burned %s = %d µLENS, want %d — a held-mint clawback is NOT a burn (its type is not in the burned list)", when, burned, wantBurned)
	}
}
