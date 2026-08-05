package economy

import (
	"context"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/mining"
)

// stake_yield_realpg_test.go — staking yield is NEW LENS, and supply has to say so.
//
// FOUND BY #400's SWEEP, REPORTED, NOT FIXED THEN. MarketplaceStore.Unstake credited
// `principal + yield` as ONE ledger row of type "unstake", which is in no counted supply list.
// Same class as the convert_to_lxc defect that #400 fixed, with the sign flipped: an uncounted
// MINT rather than an uncounted BURN.
//
// PRINCIPAL AND YIELD ARE NOT THE SAME MONEY, and conflating them is the defect:
//
//	PRINCIPAL  was already in supply when it was staked. Staking debits the wallet ("stake") and
//	           unstaking credits it back ("unstake"); neither is in the counted set NOR the burned
//	           set, which is CORRECT and symmetric — staked LENS never left circulation, it was
//	           locked. Counting the return as a mint would inflate supply on every unstake.
//	YIELD      did not exist before. computeYield() creates it. It is a mint in every sense that
//	           matters to GetTotalSupply, and it was invisible to it.
//
// So the fix is not "add unstake to the counted list" — that would count the principal too, and
// would be a worse error than the one it replaces. The row has to be SPLIT.
//
// NO MIGRATION: lens_token_ledger.type is a bare TEXT column (0019) and the only CHECK on the
// table is balance_after >= 0 (0036), so a new type is data, not schema.

// (1) THE DEFECT. Stake, let yield accrue, unstake — total supply must grow by exactly the yield
// and by nothing else.
//
// RED before the fix: total supply is unchanged, because the whole payout landed under "unstake".
func TestUnstake_YieldEntersSupply_PrincipalDoesNot(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	const ws = "ws-stake-1"
	seedWorkspace(t, pool, ws)
	ledger := mining.NewLedgerStore(pool)
	store := NewMarketplaceStore(ledger, pool)

	const minted int64 = 1_000_000_000 // 1000 LENS
	mintLENS(t, pool, ws, minted)

	total0, circ0, _ := supplyNow(t, pool)
	if total0 != minted || circ0 != minted {
		t.Fatalf("before: total=%d circ=%d, want %d/%d", total0, circ0, minted, minted)
	}

	// Stake half of it.
	const principal int64 = 500_000_000
	pos, err := store.Stake(ctx, ws, principal, 30)
	if err != nil {
		t.Fatalf("stake: %v", err)
	}

	// STAKING MOVES NOTHING IN OR OUT OF SUPPLY. The wallet falls, but the LENS is locked, not
	// destroyed — circulating is "wallets + staked" by definition.
	total1, circ1, _ := supplyNow(t, pool)
	if total1 != total0 || circ1 != circ0 {
		t.Errorf("staking changed supply: total %d→%d circ %d→%d — a stake locks LENS, it does "+
			"not mint or burn it", total0, total1, circ0, circ1)
	}

	// Backdate so real yield accrues.
	if _, err := pool.Exec(ctx,
		`UPDATE stake_positions SET started_at = started_at - interval '60 days',
		        unlocks_at = unlocks_at - interval '60 days' WHERE id = $1`, pos.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	var walletBefore int64
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(balance,0) FROM lens_token_balances WHERE workspace_id=$1`, ws).Scan(&walletBefore); err != nil {
		t.Fatalf("wallet: %v", err)
	}

	if err := store.Unstake(ctx, pos.ID, ws); err != nil {
		t.Fatalf("unstake: %v", err)
	}

	var walletAfter int64
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(balance,0) FROM lens_token_balances WHERE workspace_id=$1`, ws).Scan(&walletAfter); err != nil {
		t.Fatalf("wallet: %v", err)
	}
	credited := walletAfter - walletBefore
	yield := credited - principal
	if yield <= 0 {
		t.Fatalf("no yield accrued (credited=%d principal=%d) — the fixture proves nothing", credited, principal)
	}

	total2, circ2, _ := supplyNow(t, pool)
	if total2 != total0+yield {
		t.Errorf("total supply = %d after unstaking with %d yield, want %d — YIELD IS NEW LENS "+
			"and must enter all-time-minted; the whole payout was written under one uncounted "+
			"'unstake' row, so supply never saw it", total2, yield, total0+yield)
	}
	if circ2 != circ0+yield {
		t.Errorf("circulating = %d, want %d (nothing was burned, so circulating tracks total)", circ2, circ0+yield)
	}
	// AND NOT MORE: counting the principal too would inflate supply by 500 LENS per unstake.
	if total2 == total0+credited && yield != credited {
		t.Errorf("total grew by the WHOLE payout (%d) — the principal was counted as a mint, "+
			"which is the opposite error", credited)
	}
}

// (2) THE SPLIT IS IN THE LEDGER, not only in metadata. An auditor summing by type must be able
// to tell returned principal from created yield without parsing a JSON blob.
func TestUnstake_WritesPrincipalAndYieldAsSeparateRows(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	const ws = "ws-stake-2"
	seedWorkspace(t, pool, ws)
	mintLENS(t, pool, ws, 800_000_000)
	store := NewMarketplaceStore(mining.NewLedgerStore(pool), pool)

	pos, err := store.Stake(ctx, ws, 400_000_000, 30)
	if err != nil {
		t.Fatalf("stake: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE stake_positions SET started_at = started_at - interval '90 days',
		        unlocks_at = unlocks_at - interval '90 days' WHERE id = $1`, pos.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if err := store.Unstake(ctx, pos.ID, ws); err != nil {
		t.Fatalf("unstake: %v", err)
	}

	var principalRows, yieldRows int
	var principalSum, yieldSum int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(SUM(amount),0) FROM lens_token_ledger
		  WHERE workspace_id=$1 AND type='unstake'`, ws).Scan(&principalRows, &principalSum); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(SUM(amount),0) FROM lens_token_ledger
		  WHERE workspace_id=$1 AND type=$2`, ws, mining.TypeStakeYield).Scan(&yieldRows, &yieldSum); err != nil {
		t.Fatal(err)
	}

	if principalRows != 1 || principalSum != 400_000_000 {
		t.Errorf("'unstake' rows=%d sum=%d, want 1 row of exactly the principal 400000000 — the "+
			"returned principal and the created yield are different money", principalRows, principalSum)
	}
	if yieldRows != 1 || yieldSum <= 0 {
		t.Errorf("'%s' rows=%d sum=%d, want one positive row", mining.TypeStakeYield, yieldRows, yieldSum)
	}
}

// (3) #400's SWEEP, RE-RUN AGAINST THIS CHANGE. Every LENS type against both predicates — because
// adding a type to one set is exactly how the previous two defects were introduced.
func TestUnstake_TypeSweep_StillExactlyOnce(t *testing.T) {
	counted := map[string]bool{}
	for _, ty := range mining.CountedSupplyTypes() {
		counted[ty] = true
	}
	burned := map[string]bool{}
	for _, ty := range mining.BurnedSupplyTypes() {
		burned[ty] = true
	}

	// MINTS — new LENS. Counted, never burned.
	for _, ty := range []string{
		mining.TypeCacheMine, mining.TypeComputeMine, mining.TypeEmbeddingMine,
		mining.TypeAnnotationMine, mining.TypePatternMine, mining.TypePoolRoyalty,
		mining.TypeEvalContribution, mining.TypeRoutingPrediction,
		mining.TypeLatencyLocality, mining.TypeConfidentialCompute,
		mining.TypeStakeYield, // ← this change
	} {
		if !counted[ty] {
			t.Errorf("%q creates LENS but is not counted — total supply understates what was minted", ty)
		}
		if burned[ty] {
			t.Errorf("%q is in BOTH sets", ty)
		}
	}

	// DESTRUCTIVE — burned, never counted.
	for _, ty := range []string{mining.TypeBurn, mining.TypeStakeSlash, mining.TypeConvertToLXC} {
		if !burned[ty] {
			t.Errorf("%q destroys LENS but is not burned", ty)
		}
		if counted[ty] {
			t.Errorf("%q is in BOTH sets — a double count", ty)
		}
	}

	// MOVES — a counterparty holds it, or it never entered supply. Neither set.
	// ⚠ "unstake" and "stake" belong HERE and must stay here: the principal was already in supply
	// when it was staked, so returning it is not a mint. That is precisely why the fix splits the
	// row instead of counting "unstake".
	for _, ty := range []string{
		"stake", "unstake",
		mining.TypeTransfer, mining.TypeTransferIn, mining.TypeTransferOut,
		mining.TypeStakeLock, mining.TypeStakeRelease,
		mining.TypePoolRoyaltyHeld, mining.TypePoolRoyaltyRevoked,
		mining.TypeEvalContributionHeld, mining.TypeEvalContributionRevoked,
		"marketplace_listing", "marketplace_buy", "marketplace_fee",
		"marketplace_refund", "marketplace_unsold_refund",
	} {
		if counted[ty] {
			t.Errorf("%q is not a mint but is counted — total supply would overstate", ty)
		}
		if burned[ty] {
			t.Errorf("%q does not destroy LENS but is burned — circulating would understate", ty)
		}
	}
}

// (4) THE CONSUMER, as #400 did. ComputeFairRate reads both supplies, so a change to either moves
// the number that prices conversions. FairRate = circulating/totalMinted once the peg cancels.
//
// Yield lands in BOTH (it is minted, and nothing is burned), so the rate moves toward 1 rather
// than away — and with no burns at all it does not move whatsoever.
func TestUnstake_FairRateUnderTheFix(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	const ws = "ws-stake-3"
	seedWorkspace(t, pool, ws)
	mintLENS(t, pool, ws, 1_000_000_000)
	ledger := mining.NewLedgerStore(pool)
	eng := NewRateEngine(ledger, pool)
	store := NewMarketplaceStore(ledger, pool)

	before, err := eng.ComputeFairRate(ctx)
	if err != nil {
		t.Fatalf("rate: %v", err)
	}

	pos, err := store.Stake(ctx, ws, 400_000_000, 30)
	if err != nil {
		t.Fatalf("stake: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE stake_positions SET started_at = started_at - interval '90 days',
		        unlocks_at = unlocks_at - interval '90 days' WHERE id = $1`, pos.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Unstake(ctx, pos.ID, ws); err != nil {
		t.Fatalf("unstake: %v", err)
	}

	after, err := eng.ComputeFairRate(ctx)
	if err != nil {
		t.Fatalf("rate: %v", err)
	}

	// With nothing burned, circulating == total both before and after, so FairRate is exactly 1.0
	// on both sides. The yield raises both by the same amount and the ratio is unmoved.
	if before.FairRate != 1.0 || after.FairRate != 1.0 {
		t.Errorf("FairRate before=%v after=%v, want 1.0 both — with no burns, circulating tracks "+
			"total exactly and a mint moves neither side of the ratio", before.FairRate, after.FairRate)
	}
	// And the audit value that gets PERSISTED to conversion_rate_history must reflect the new
	// circulating, not the pre-yield one.
	if after.Circulating <= before.Circulating {
		t.Errorf("RateComputation.Circulating did not grow with the minted yield: %v → %v",
			before.Circulating, after.Circulating)
	}
}

var _ = time.Now
