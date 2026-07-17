package outputverify_test

// attribution_moneyfrozen_integration_test.go — PROPERTY 6: attribution ≠ settlement (money-frozen).
//
// The obligation: establish REAL money state (mint held → settle → supply), then perform an
// attribution write, and prove EVERY money column across the ledger / held / mint / supply surface
// is BYTE-IDENTICAL before and after. Attribution must move nothing — no ledger row, no balance, no
// supply, no burned total.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/outputverify"
)

// moneyDump canonicalizes EVERY money column so two snapshots are byte-comparable.
func moneyDump(t *testing.T, pool *pgxpool.Pool, ledger *mining.LedgerStore) string {
	t.Helper()
	ctx := context.Background()
	var out strings.Builder

	out.WriteString("# lens_token_ledger: workspace_id|amount|balance_after|type\n")
	lines := dumpRows(t, pool, `SELECT workspace_id, amount, balance_after, type FROM lens_token_ledger`,
		func(scan func(...any) error) string {
			var ws, ty string
			var amt, ba int64
			_ = scan(&ws, &amt, &ba, &ty)
			return fmt.Sprintf("%s|%d|%d|%s", ws, amt, ba, ty)
		})
	out.WriteString(lines)

	out.WriteString("# lens_token_balances: workspace_id|balance|held_balance|lifetime_earned|lifetime_spent\n")
	blines := dumpRows(t, pool, `SELECT workspace_id, balance, held_balance, lifetime_earned, lifetime_spent FROM lens_token_balances`,
		func(scan func(...any) error) string {
			var ws string
			var bal, held, earned, spent int64
			_ = scan(&ws, &bal, &held, &earned, &spent)
			return fmt.Sprintf("%s|%d|%d|%d|%d", ws, bal, held, earned, spent)
		})
	out.WriteString(blines)

	supply, err := ledger.GetTotalSupply(ctx)
	if err != nil {
		t.Fatal(err)
	}
	burned, err := ledger.GetTotalBurned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&out, "# totals: supply=%d burned=%d\n", supply, burned)
	return out.String()
}

func dumpRows(t *testing.T, pool *pgxpool.Pool, q string, f func(scan func(...any) error) string) string {
	t.Helper()
	rows, err := pool.Query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		lines = append(lines, f(rows.Scan))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n"
}

// TestAttribution_MoneyFrozen settles a mint, then proves an attribution write moves no money.
func TestAttribution_MoneyFrozen(t *testing.T) {
	ctx := context.Background()
	pool := ovTestPool(t)
	ledger := mining.NewLedgerStore(pool)
	const ws = "ws-money-frozen"

	// Establish REAL money state: credit a held mint, then SETTLE it (held → spendable, enters supply).
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.CreditHeldTx(ctx, tx, ws, 50_000, mining.TypePoolRoyaltyHeld, "seed held mint", nil); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.FinalizeHeldTxAs(ctx, tx2, ws, 50_000, mining.TypePoolRoyalty, "settle", nil); err != nil {
		_ = tx2.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	before := moneyDump(t, pool, ledger)
	// Non-vacuous: the settle must have produced real money state (ledger rows for ws), so the
	// byte-identical proof below is freezing SOMETHING, not comparing empty to empty.
	if !strings.Contains(before, ws) {
		t.Fatalf("precondition: money state must be non-empty after the settle; dump:\n%s", before)
	}

	// The operation under test: an attribution write by the producer (must move NO money).
	seedProducer(t, outputverify.NewWriter(pool), "oid-money-frozen", ws, "moneyfrozen")
	owned, recorded, _, err := outputverify.NewAttributionWriter(pool).RecordAttributionIfOwned(ctx,
		outputverify.Attribution{OutputID: "oid-money-frozen", WorkspaceID: ws, TargetKind: "pr", TargetRef: "pr://money"})
	if err != nil || !owned || !recorded {
		t.Fatalf("attribution write: owned=%v recorded=%v err=%v, want owned+recorded", owned, recorded, err)
	}

	after := moneyDump(t, pool, ledger)
	if before != after {
		t.Fatalf("MONEY MOVED across an attribution write — attribution != settlement is VIOLATED.\n--- BEFORE ---\n%s\n--- AFTER ---\n%s", before, after)
	}
}
