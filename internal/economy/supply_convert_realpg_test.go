package economy

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/migrations"
)

// supply_convert_realpg_test.go — a LENS→LXC conversion DESTROYS LENS, and the supply
// accounting has to say so.
//
// WHAT THE TWO FUNCTIONS MEAN (established before either was changed):
//
//	GetTotalSupply       — ALL-TIME MINTED. Every µLENS ever created by a mining track or a
//	                       settled royalty. It is a HIGH-WATER MARK: it must never fall,
//	                       because "how much work was ever rewarded" cannot decrease. Burning
//	                       a token does not un-mint it.
//	GetCirculatingSupply — total minted MINUS what has been destroyed. What is actually held
//	                       in wallets + staked right now.
//
// A convert_to_lxc row is a DEBIT with NO counterparty credit anywhere: ConvertLENStoLXC
// debits the workspace's LENS and credits only LXC — a different token, on a different
// ledger. No treasury, no house account holds that LENS afterwards (a house account was
// considered and rejected: LXC is a liability, and spending it is what discharges it). So
// the LENS is GONE, not moved. That makes it a BURN, and burns belong in circulating's
// subtrahend — never in total's, which would retroactively un-mint work that really happened.
//
// THE DEFECT (flagged by #399 and deliberately left): convert_to_lxc appears in NEITHER set.
// It is excluded from total twice over (not in countedSupplyTypeList, and negative, which the
// `amount > 0` filter drops independently), and it is absent from the burned list, which
// names only TypeBurn and TypeStakeSlash. So counted circulating stays FLAT while real wallet
// LENS falls — the gap widening with every conversion.
//
// WHY IT IS NOT MERELY AN ACCOUNTING NICETY: ComputeFairRate reads BOTH, and the algebra
// collapses to
//
//	FairRate = circulating / totalMinted        (the $0.10 peg cancels out)
//
// so an overstated circulating overstates the rate that prices the NEXT conversion. The
// error feeds the number that causes more of the error.

func supplyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv("LENS_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	name := fmt.Sprintf("lens_supply_%d", time.Now().UnixNano())
	ac, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ac.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		_ = ac.Close(ctx)
		t.Fatal(err)
	}
	_ = ac.Close(ctx)
	u, _ := url.Parse(admin)
	u.Path = "/" + name
	mc, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dbmigrate.Run(ctx, mc, migrations.FS); err != nil {
		_ = mc.Close(ctx)
		t.Fatal(err)
	}
	_ = mc.Close(ctx)
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		c, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			return
		}
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = c.Close(context.Background())
	})
	return pool
}

// seedWorkspace makes a workspace the FK constraints will accept.
func seedWorkspace(t *testing.T, pool *pgxpool.Pool, ws string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1 || ':')
		 ON CONFLICT (id) DO NOTHING`, ws); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
}

// mintLENS credits a COUNTED mint type, the way a settled mining track does.
func mintLENS(t *testing.T, pool *pgxpool.Pool, ws string, amount int64) {
	t.Helper()
	if err := mining.NewLedgerStore(pool).Credit(context.Background(), ws, amount,
		mining.TypeCacheMine, "test mint", nil); err != nil {
		t.Fatalf("mint %d: %v", amount, err)
	}
}

func supplyNow(t *testing.T, pool *pgxpool.Pool) (total, circulating, burned int64) {
	t.Helper()
	ctx := context.Background()
	ls := mining.NewLedgerStore(pool)
	var err error
	if total, err = ls.GetTotalSupply(ctx); err != nil {
		t.Fatalf("total: %v", err)
	}
	if circulating, err = ls.GetCirculatingSupply(ctx); err != nil {
		t.Fatalf("circulating: %v", err)
	}
	if burned, err = ls.GetTotalBurned(ctx); err != nil {
		t.Fatalf("burned: %v", err)
	}
	return total, circulating, burned
}

// (1) THE NUMBERS. Mint N, convert M, and assert both supplies are what the definitions say
// — before and after.
//
// RED on shipped code: circulating stays at N after the conversion, so the ledger claims N
// µLENS are in wallets when only N−M are.
func TestSupply_ConversionBurnsLENS_CirculatingFalls_TotalDoesNot(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	const ws = "ws-supply-1"
	seedWorkspace(t, pool, ws)

	const minted int64 = 900_000_000 // 900 LENS

	mintLENS(t, pool, ws, minted)
	total0, circ0, burned0 := supplyNow(t, pool)
	if total0 != minted || circ0 != minted || burned0 != 0 {
		t.Fatalf("before: total=%d circ=%d burned=%d, want %d/%d/0", total0, circ0, burned0, minted, minted)
	}

	// Convert. With an empty conversion_rate_history CurrentRate returns the Phase-1 floor
	// (1.0 LENS per LXC), so the LENS cost equals the LXC minted — see the rate test below.
	const lxc int64 = 300_000_000 // 300 LXC
	res, err := NewDualTokenStore(mining.NewLedgerStore(pool), pool, NewRateEngine(mining.NewLedgerStore(pool), pool)).
		ConvertLENStoLXC(ctx, ws, lxc)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if res.LENSSpent <= 0 {
		t.Fatalf("conversion spent %d µLENS — the fixture proves nothing", res.LENSSpent)
	}

	total1, circ1, burned1 := supplyNow(t, pool)

	// TOTAL is a high-water mark: burning does not un-mint the work that was rewarded.
	if total1 != minted {
		t.Errorf("total supply = %d after a conversion, want %d unchanged — total is ALL-TIME "+
			"MINTED and a burn must never retroactively un-mint work", total1, minted)
	}
	// CIRCULATING must fall by exactly what left the wallet.
	wantCirc := minted - res.LENSSpent
	if circ1 != wantCirc {
		t.Errorf("circulating = %d after converting %d µLENS, want %d — the conversion debit is "+
			"in NEITHER the counted set NOR the burned set, so counted circulating stays flat "+
			"while the wallet actually fell", circ1, res.LENSSpent, wantCirc)
	}
	// And the burned readout must agree with circulating, or two screens disagree.
	if burned1 != res.LENSSpent {
		t.Errorf("burned = %d, want %d — GetEconomyStats reports total−burned and must equal "+
			"GetCirculatingSupply; a divergence here is two different 'circulating' numbers",
			burned1, res.LENSSpent)
	}
	if total1-burned1 != circ1 {
		t.Errorf("total−burned = %d but circulating = %d — the two public readouts disagree",
			total1-burned1, circ1)
	}

	// The wallet is the ground truth the accounting is supposed to describe.
	var wallet int64
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(balance,0) FROM lens_token_balances WHERE workspace_id=$1`, ws).Scan(&wallet); err != nil {
		t.Fatalf("wallet: %v", err)
	}
	if circ1 != wallet {
		t.Errorf("circulating = %d but the only wallet holds %d — circulating supply is "+
			"supposed to BE what is in wallets", circ1, wallet)
	}
}

// (2) THE CONSUMER. ComputeFairRate reads both numbers, so the defect corrupts the price of
// the next conversion. FairRate = circulating/total once the peg cancels, so a conversion
// must move it DOWN: fewer LENS remain against the same all-time work, so each is worth more
// and fewer are needed per LXC — exactly what ComputeFairRate's own doc comment claims.
//
// RED on shipped code: FairRate stays at 1.0 because circulating never moved.
func TestComputeFairRate_FallsAfterAConversion(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	const ws = "ws-supply-2"
	seedWorkspace(t, pool, ws)

	const minted int64 = 1_000_000_000 // 1000 LENS
	mintLENS(t, pool, ws, minted)

	eng := NewRateEngine(mining.NewLedgerStore(pool), pool)
	before, err := eng.ComputeFairRate(ctx)
	if err != nil {
		t.Fatalf("rate before: %v", err)
	}
	// Nothing burned yet ⇒ circulating == total ⇒ FairRate == 1.0 exactly.
	if before.FairRate != 1.0 {
		t.Fatalf("FairRate before = %v, want exactly 1.0 (circulating==total) — the fixture is wrong", before.FairRate)
	}

	const lxc int64 = 200_000_000 // 200 LXC
	res, err := NewDualTokenStore(mining.NewLedgerStore(pool), pool, eng).ConvertLENStoLXC(ctx, ws, lxc)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	after, err := eng.ComputeFairRate(ctx)
	if err != nil {
		t.Fatalf("rate after: %v", err)
	}

	wantFair := float64(minted-res.LENSSpent) / float64(minted)
	if after.FairRate >= before.FairRate {
		t.Errorf("FairRate did not fall: before=%v after=%v (want %v) — a conversion burns LENS, "+
			"so the survivors are backed by proportionally more work and the rate MUST fall. "+
			"This is the loop: an overstated circulating overstates the rate that prices the "+
			"NEXT conversion", before.FairRate, after.FairRate, wantFair)
	}
	if diff := after.FairRate - wantFair; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("FairRate = %v, want %v (= circulating/total after the burn)", after.FairRate, wantFair)
	}
	if after.Circulating != mining.MicroToFloat(minted-res.LENSSpent) {
		t.Errorf("RateComputation.Circulating = %v, want %v — this value is PERSISTED to "+
			"conversion_rate_history as the audit record of what priced the rate",
			after.Circulating, mining.MicroToFloat(minted-res.LENSSpent))
	}
}

// (3) TOTAL SUPPLY IS NOT DOUBLE-COUNTED, and the `amount > 0` filter is not load-bearing for
// the fix. The positive-rows-only rule exists because some COUNTED types can also appear as
// negative rows; removing it is not part of this change, so pin that the fix rides on top of
// it unchanged.
func TestSupply_NoDoubleCount_AndTotalIsMonotonic(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	const ws = "ws-supply-3"
	seedWorkspace(t, pool, ws)

	mintLENS(t, pool, ws, 500_000_000)
	t0, _, _ := supplyNow(t, pool)

	// A plain burn and a conversion must each reduce circulating ONCE, never twice, and must
	// leave total alone.
	if err := mining.NewLedgerStore(pool).Burn(ctx, ws, 50_000_000, "test burn"); err != nil {
		t.Fatalf("burn: %v", err)
	}
	res, err := NewDualTokenStore(mining.NewLedgerStore(pool), pool, NewRateEngine(mining.NewLedgerStore(pool), pool)).
		ConvertLENStoLXC(ctx, ws, 100_000_000)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	t1, c1, b1 := supplyNow(t, pool)
	if t1 != t0 {
		t.Errorf("total moved from %d to %d — burns and conversions must not change all-time minted", t0, t1)
	}
	wantBurned := int64(50_000_000) + res.LENSSpent
	if b1 != wantBurned {
		t.Errorf("burned = %d, want %d (one plain burn + one conversion, each counted ONCE)", b1, wantBurned)
	}
	if c1 != t0-wantBurned {
		t.Errorf("circulating = %d, want %d", c1, t0-wantBurned)
	}
}

// (4) THE SWEEP, ASSERTED. Every LENS ledger type that REMOVES µLENS from a wallet must be in
// exactly one of the two sets, and no type may be in both. Written as a table so a type added
// later fails here rather than silently falling through both predicates — which is precisely
// how convert_to_lxc got missed.
func TestSupply_EveryDestructiveTypeIsAccountedExactlyOnce(t *testing.T) {
	counted := map[string]bool{}
	for _, ty := range mining.CountedSupplyTypes() {
		counted[ty] = true
	}
	burned := map[string]bool{}
	for _, ty := range mining.BurnedSupplyTypes() {
		burned[ty] = true
	}

	// Types that DESTROY LENS — no counterparty holds it afterwards.
	for _, ty := range []string{
		mining.TypeBurn,
		mining.TypeStakeSlash,
		mining.TypeConvertToLXC,
	} {
		if !burned[ty] {
			t.Errorf("%q destroys LENS but is not in the burned set — circulating supply will "+
				"overstate what is in wallets", ty)
		}
		if counted[ty] {
			t.Errorf("%q is in BOTH sets — it would be added to total and subtracted from "+
				"circulating, which is a double count", ty)
		}
	}

	// Types that MOVE existing LENS (a counterparty is credited) or that never entered supply
	// must be in NEITHER set.
	for _, ty := range []string{
		mining.TypeTransfer, mining.TypeTransferIn, mining.TypeTransferOut,
		mining.TypeStakeLock, mining.TypeStakeRelease,
		mining.TypePoolRoyaltyHeld, mining.TypePoolRoyaltyRevoked,
		mining.TypeEvalContributionHeld, mining.TypeEvalContributionRevoked,
		"marketplace_listing", "marketplace_buy", "marketplace_fee", "marketplace_refund",
	} {
		if burned[ty] {
			t.Errorf("%q moves LENS rather than destroying it, but is in the burned set — "+
				"circulating would understate wallets", ty)
		}
		if counted[ty] {
			t.Errorf("%q is not a mint but is in the counted set — total supply would overstate "+
				"the work ever rewarded", ty)
		}
	}
}

// (5) WHAT THIS DOES TO A PRICE A USER COULD ALREADY HAVE SEEN.
//
// Two facts bound the blast radius, and both are asserted here rather than argued:
//
//	(a) With conversion_rate_history EMPTY, CurrentRate returns Phase1FloorRate (1.0), so
//	    ConvertLENStoLXC prices at exactly 1.0 LENS per LXC NO MATTER WHAT SUPPLY SAYS. The
//	    supply numbers do not reach a converting user until an admin calls ApproveRate. So on
//	    a deployment with no approved rate, this fix changes no price anyone can be quoted.
//	(b) The approved rate this fix can produce is bounded to [Phase1FloorRate, 1.05]. FairRate
//	    = circulating/total ≤ 1 always, the spread multiplies by 1.05, and the floor clamps
//	    below — so the ENTIRE dynamic range the fix moves within is 5%. Past ~4.76% of
//	    all-time-minted burned (1 − 1/1.05), the floor pins the rate at exactly 1.0 and the
//	    fix cannot move it at all.
func TestConversionPrice_FloorGovernsUntilARateIsApproved(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	const ws = "ws-supply-4"
	seedWorkspace(t, pool, ws)
	mintLENS(t, pool, ws, 1_000_000_000) // 1000 LENS

	eng := NewRateEngine(mining.NewLedgerStore(pool), pool)

	// (a) No approved rate ⇒ the floor prices the conversion, supply notwithstanding.
	rate, err := eng.CurrentRate(ctx)
	if err != nil {
		t.Fatalf("CurrentRate: %v", err)
	}
	if rate != Phase1FloorRate {
		t.Fatalf("CurrentRate with empty history = %v, want the Phase-1 floor %v", rate, Phase1FloorRate)
	}
	const lxc int64 = 100_000_000
	res, err := NewDualTokenStore(mining.NewLedgerStore(pool), pool, eng).ConvertLENStoLXC(ctx, ws, lxc)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if res.LENSSpent != lxc || res.Rate != Phase1FloorRate {
		t.Errorf("priced at rate=%v cost=%d for %d µLXC; want the floor (rate 1.0, cost == lxc) — "+
			"with no approved rate the supply fix cannot move what a user pays", res.Rate, res.LENSSpent, lxc)
	}

	// (b) Even once burned, the ADMIN rate stays inside [floor, floor×(1+spread)].
	comp, err := eng.ComputeFairRate(ctx)
	if err != nil {
		t.Fatalf("ComputeFairRate: %v", err)
	}
	if comp.AdminRate < Phase1FloorRate || comp.AdminRate > Phase1FloorRate*(1+ConversionSpread) {
		t.Errorf("AdminRate = %v, outside [%v, %v] — the floor and the spread are supposed to "+
			"bound every rate this computation can produce", comp.AdminRate,
			Phase1FloorRate, Phase1FloorRate*(1+ConversionSpread))
	}

	// Past the point where the spread can no longer lift FairRate over the floor, the rate is
	// pinned at exactly the floor — the fix provably cannot move it further.
	pinFraction := 1 - 1/(1+ConversionSpread)
	if err := mining.NewLedgerStore(pool).Burn(ctx, ws,
		int64(float64(1_000_000_000)*(pinFraction+0.01)), "burn past the pin point"); err != nil {
		t.Fatalf("burn: %v", err)
	}
	pinned, err := eng.ComputeFairRate(ctx)
	if err != nil {
		t.Fatalf("ComputeFairRate after burn: %v", err)
	}
	if pinned.AdminRate != Phase1FloorRate || !pinned.Floored {
		t.Errorf("after burning past %.4f of all-time minted, AdminRate = %v (floored=%v); "+
			"want exactly the floor %v with Floored=true",
			pinFraction, pinned.AdminRate, pinned.Floored, Phase1FloorRate)
	}
}

// (6) THE ALIAS IS NOT FREE. LENSTypeConvertToLXC became mining.TypeConvertToLXC so the string
// the ledger WRITES and the string supply SUBTRACTS cannot drift. That removes one failure
// mode and introduces another: renaming the mining constant's VALUE would silently orphan
// every convert_to_lxc row already in the ledger — they would stop being subtracted, and the
// exact defect this change fixes would return with no test failing. Pin the literal.
func TestConvertLedgerTypeString_IsPinned(t *testing.T) {
	if LENSTypeConvertToLXC != "convert_to_lxc" {
		t.Errorf("LENSTypeConvertToLXC = %q, want %q — this string is written into "+
			"lens_token_ledger.type and matched by burnedSupplyTypeList; changing it strands "+
			"every existing row outside the burned set", LENSTypeConvertToLXC, "convert_to_lxc")
	}
	if mining.TypeConvertToLXC != LENSTypeConvertToLXC {
		t.Errorf("alias drift: mining.TypeConvertToLXC=%q economy.LENSTypeConvertToLXC=%q",
			mining.TypeConvertToLXC, LENSTypeConvertToLXC)
	}
}
