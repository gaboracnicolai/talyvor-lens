package earnings

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/talyvor/lens/internal/mining"
)

// reader_realpg_test.go — the read half of W4.6.1 step 7, against real Postgres and the real ledger.
// Positive-controlled by scripts/w461-earnings-reader-controls-h3n8.py (the R-series).

func allGatesOn() Gates {
	return Gates{EconomyEnabled: true, PoolRoyaltyMintingEnabled: true, CachePoolableEnabled: true, DistillPoolableEnabled: true}
}

func TestR1_ContributionAndCapitalAreSeparateSubtotals(t *testing.T) {
	pool := harness(t)
	ls := mining.NewLedgerStore(pool)
	r := NewReader(pool)
	ctx := context.Background()
	const ws = "w461-r1"

	// One contribution earning, one capital earning, and three credits that are not income at all.
	if err := ls.Credit(ctx, ws, 6_000_000, mining.TypePoolRoyalty, "reuse of an answer", nil); err != nil {
		t.Fatalf("royalty: %v", err)
	}
	if err := ls.Credit(ctx, ws, 2_000_000, mining.TypeStakeYield, "yield on locked LENS", nil); err != nil {
		t.Fatalf("yield: %v", err)
	}
	for _, typ := range []string{"unstake", "marketplace_buy", mining.TypeTransferIn} {
		if err := withTx(ctx, pool, func(tx pgx.Tx) error {
			return ls.CreditTx(ctx, tx, ws, 50_000_000, typ, "not income", nil)
		}); err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
	}

	got, err := r.ForWorkspace(ctx, ws, allGatesOn())
	if err != nil {
		t.Fatalf("ForWorkspace: %v", err)
	}

	if got.ContributionSettledULENS != 6_000_000 {
		t.Errorf("[R1-CONTRIB] contribution_settled=%d, want 6000000. 150,000,000 µLENS of gifts, "+
			"purchases and returned principal sit in this ledger; if any of it leaked in, the sentence "+
			"'your answers earned this' is false.", got.ContributionSettledULENS)
	}
	if got.CapitalSettledULENS != 2_000_000 {
		t.Errorf("[R1-CAPITAL] capital_settled=%d, want 2000000 (the stake yield)", got.CapitalSettledULENS)
	}
	if got.SettledULENS != 8_000_000 {
		t.Errorf("[R1-TOTAL] settled=%d, want 8000000 = contribution + capital", got.SettledULENS)
	}
	// The dollars, at the published peg: 6 LENS ÷ 10 LENS/$ = $0.60.
	if got.ContributionSettledUSDAtPeg != 0.60 {
		t.Errorf("[R1-USD] contribution_settled_usd_at_peg=%v, want 0.60 (6 LENS at %v LENS/$)",
			got.ContributionSettledUSDAtPeg, got.LENSPerUSD)
	}
	// FLOOR: the by-type breakdown must actually enumerate what it summed.
	if len(got.ByType) != 5 {
		t.Errorf("[R1-BYTYPE] by_type has %d lines for 5 distinct ledger types — a summary that cannot "+
			"show its composition is the shape this package exists to avoid: %+v", len(got.ByType), got.ByType)
	}
}

func TestR2_OneWorkspacesEarningsAreNotAnothers(t *testing.T) {
	pool := harness(t)
	ls := mining.NewLedgerStore(pool)
	r := NewReader(pool)
	ctx := context.Background()

	if err := ls.Credit(ctx, "w461-r2-a", 9_000_000, mining.TypePoolRoyalty, "A earned", nil); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if err := ls.Credit(ctx, "w461-r2-b", 1_000_000, mining.TypePoolRoyalty, "B earned", nil); err != nil {
		t.Fatalf("seed B: %v", err)
	}

	a, err := r.ForWorkspace(ctx, "w461-r2-a", allGatesOn())
	if err != nil {
		t.Fatalf("A: %v", err)
	}
	b, err := r.ForWorkspace(ctx, "w461-r2-b", allGatesOn())
	if err != nil {
		t.Fatalf("B: %v", err)
	}
	if a.ContributionSettledULENS != 9_000_000 || b.ContributionSettledULENS != 1_000_000 {
		t.Fatalf("[R2] scoping is wrong: A=%d (want 9000000), B=%d (want 1000000). If either is "+
			"10000000 the query is summing the whole table.",
			a.ContributionSettledULENS, b.ContributionSettledULENS)
	}
}

// TestR3_HeldComesFromTheBalanceColumnNotFromSummingHeldRows is the trap this package was written
// around. Finalize decrements held_balance and writes a POSITIVE settled row; it does NOT write a
// negative `*_held` row. So a surface that sums `*_held` ledger rows reports every mint ever held,
// including every one already paid out — and it grows monotonically forever.
func TestR3_HeldComesFromTheBalanceColumnNotFromSummingHeldRows(t *testing.T) {
	pool := harness(t)
	ls := mining.NewLedgerStore(pool)
	r := NewReader(pool)
	ctx := context.Background()
	const ws = "w461-r3"

	// Hold 5 LENS, then settle 4 of it. Held should end at 1 LENS.
	if err := withTx(ctx, pool, func(tx pgx.Tx) error {
		return ls.CreditHeldTx(ctx, tx, ws, 5_000_000, mining.TypePoolRoyaltyHeld, "held royalty", nil)
	}); err != nil {
		t.Fatalf("CreditHeldTx: %v", err)
	}
	if err := withTx(ctx, pool, func(tx pgx.Tx) error {
		return ls.FinalizeHeldTx(ctx, tx, ws, 4_000_000, "settled", nil)
	}); err != nil {
		t.Fatalf("FinalizeHeldTx: %v", err)
	}

	got, err := r.ForWorkspace(ctx, ws, allGatesOn())
	if err != nil {
		t.Fatalf("ForWorkspace: %v", err)
	}

	// What the naive surface would have reported, computed here so the two are compared rather than
	// asserted about.
	var rowsSum int64
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount),0)::bigint FROM lens_token_ledger
		 WHERE workspace_id=$1 AND type='pool_royalty_held'`, ws).Scan(&rowsSum); err != nil {
		t.Fatalf("rows sum: %v", err)
	}

	if rowsSum != 5_000_000 {
		t.Fatalf("[R3-PREMISE] summing pool_royalty_held rows gives %d, expected the full 5000000 — "+
			"if finalize HAS started writing a negative held row, the trap this test names is gone and "+
			"the comment on Summary.HeldULENS is now wrong.", rowsSum)
	}
	if got.HeldULENS != 1_000_000 {
		t.Fatalf("[R3] held_ulens=%d, want 1000000. The rows-sum says %d — %.0f× too much, because "+
			"finalize does not reverse the held row. Read the column.",
			got.HeldULENS, rowsSum, float64(rowsSum)/1_000_000.0)
	}
	if got.HeldULENS == rowsSum {
		t.Fatalf("[R3-SAME] held_ulens equals the rows-sum, so this test cannot tell the two apart " +
			"and proves nothing about which one is being read")
	}
	// And the settled half landed where it should.
	if got.ContributionSettledULENS != 4_000_000 {
		t.Errorf("[R3-SETTLED] contribution_settled=%d, want 4000000", got.ContributionSettledULENS)
	}
}

// TestR4_AZeroWithEarningOffIsDistinguishableFromAZeroWithEarningOn — the default deployment mints
// no royalty at all, so 0 is the correct answer and says nothing about the workspace. A surface that
// cannot tell those apart reports an operator setting as a measurement.
func TestR4_AZeroWithEarningOffIsDistinguishableFromAZeroWithEarningOn(t *testing.T) {
	pool := harness(t)
	r := NewReader(pool)
	ctx := context.Background()
	const ws = "w461-r4-never-earned"

	off, err := r.ForWorkspace(ctx, ws, Gates{}) // every switch at its shipped default: false
	if err != nil {
		t.Fatalf("off: %v", err)
	}
	on, err := r.ForWorkspace(ctx, ws, allGatesOn())
	if err != nil {
		t.Fatalf("on: %v", err)
	}

	if off.ContributionSettledULENS != 0 || on.ContributionSettledULENS != 0 {
		t.Fatalf("[R4-PREMISE] this workspace has earned nothing; got off=%d on=%d",
			off.ContributionSettledULENS, on.ContributionSettledULENS)
	}
	if off.EarningEnabled {
		t.Errorf("[R4-OFF] earning_enabled is true with every gate at its shipped default (false)")
	}
	if !on.EarningEnabled {
		t.Errorf("[R4-ON] earning_enabled is false with every gate on — the flag never says yes, so it " +
			"cannot distinguish anything")
	}
	if len(off.DisabledGates) != 4 {
		t.Errorf("[R4-GATES] disabled_gates lists %v; all four switches are off, and a reader needs to "+
			"know WHICH to turn on", off.DisabledGates)
	}
	if len(on.DisabledGates) != 0 {
		t.Errorf("[R4-GATES-ON] disabled_gates is %v with everything on", on.DisabledGates)
	}
}

// TestR5_AnUnknownLedgerTypeIsReportedRatherThanDropped — the runtime half of the completeness
// guard, and the only one that can see a bare literal nobody declared as a constant.
func TestR5_AnUnknownLedgerTypeIsReportedRatherThanDropped(t *testing.T) {
	pool := harness(t)
	ls := mining.NewLedgerStore(pool)
	r := NewReader(pool)
	ctx := context.Background()
	const ws = "w461-r5"

	if err := ls.Credit(ctx, ws, 3_000_000, mining.TypePoolRoyalty, "real", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := withTx(ctx, pool, func(tx pgx.Tx) error {
		return ls.CreditTx(ctx, tx, ws, 7_000_000, "some_future_mint", "a type nobody classified", nil)
	}); err != nil {
		t.Fatalf("unknown: %v", err)
	}

	got, err := r.ForWorkspace(ctx, ws, allGatesOn())
	if err != nil {
		t.Fatalf("ForWorkspace: %v", err)
	}
	if len(got.UnclassifiedTypes) != 1 || got.UnclassifiedTypes[0] != "some_future_mint" {
		t.Fatalf("[R5] unclassified_types=%v — an unknown ledger type must be REPORTED. Silently worth "+
			"zero is how a new mint becomes invisible on a customer's earnings screen.", got.UnclassifiedTypes)
	}
	// It must not be quietly counted either — that is the opposite error.
	if got.ContributionSettledULENS != 3_000_000 {
		t.Fatalf("[R5-COUNTED] contribution_settled=%d, want 3000000 — the unclassified type was "+
			"summed into earnings, which is worse than dropping it", got.ContributionSettledULENS)
	}
}
