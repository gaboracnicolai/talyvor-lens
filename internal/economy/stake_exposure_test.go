package economy

// stake_exposure_test.go — W6.3.3: what the staking product actually mints, made executable.
//
// ⚠ THIS FILE CHANGES NOTHING AND TUNES NOTHING. W6.3.3 is explicit: "MEASURE FIRST AND REPORT …
// That measurement is the item; do not tune the APY" and "DO NOT CHANGE THE APY, THE LOCK TIERS OR
// ANY THRESHOLD". The APY constants, the lock tiers and the yield formula are read here, never
// written. docs/stake-exposure-measured.md carries the report and the decision.
//
// ⚠ THE HEADLINE THE ARITHMETIC GIVES: the yield accrual is NOT CAPPED AT THE LOCK TERM. The
// unstake path computes `computeYield(pos.Amount, pos.APY, time.Since(pos.StartedAt))` — elapsed
// since the position STARTED, with no ceiling at `unlocks_at`. So the lock is a MINIMUM HOLD, not a
// maximum accrual window, and the mint per position grows at the full APY indefinitely. A 180-day
// position at 20% mints 9.86% of principal if unstaked on the day it unlocks — and 100% of
// principal if it is left for five years.

import (
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/mining"
)

// ⚠ THE ACCRUAL WINDOW IS UNBOUNDED IN TIME. This is a CHARACTERIZATION of today's behaviour.
//
// ⚠ IF THIS GOES RED BECAUSE THE YIELD WAS CAPPED AT unlocks_at, THAT IS THE FIX LANDING AND NOT A
// BREAKAGE — delete this test and the "unbounded" half of docs/stake-exposure-measured.md. It is
// written as a test rather than a comment so the fact is re-measured every CI run instead of
// resting on a sentence somebody has to trust.
func TestMeasured_StakeYieldIsNotCappedAtTheLockTerm(t *testing.T) {
	// ⚠ THE WINDOW IS CHOSEN AT THE CALL SITE, NOT INSIDE computeYield — AND CONTROL L1 IS HOW I
	// FOUND THAT THIS TEST COULD NOT SEE IT. The first version only exercised computeYield's
	// arithmetic directly, so capping the unstake path's window to
	// `pos.UnlocksAt.Sub(pos.StartedAt)` — which is THE FIX — left it GREEN. The claim this test
	// makes is about the UNSTAKE PATH, so it has to read the unstake path.
	src, rerr := os.ReadFile("marketplace.go")
	if rerr != nil {
		t.Fatalf("read marketplace.go: %v", rerr)
	}
	const uncapped = "computeYield(pos.Amount, pos.APY, time.Since(pos.StartedAt))"
	if !strings.Contains(string(src), uncapped) {
		t.Fatalf("the unstake path no longer computes yield over %s.\n\n⚠ IF THE WINDOW IS NOW "+
			"BOUNDED BY unlocks_at, THAT IS THE FIX LANDING AND NOT A BREAKAGE — delete this test "+
			"and the 'unbounded' half of docs/stake-exposure-measured.md.", uncapped)
	}
	// Two-sided: the arithmetic below is only meaningful if the path really calls this function.
	if strings.Count(string(src), "computeYield(") < 2 {
		t.Fatal("computeYield has fewer than two references (its definition plus at least one " +
			"caller) — this census has gone blind and its verdict would be about the search")
	}

	const principal = 1_000_000 // 1 LENS in µLENS
	atTerm := computeYield(principal, APY180, 180*24*time.Hour)
	atFiveYears := computeYield(principal, APY180, 5*365*24*time.Hour)

	if atFiveYears <= atTerm {
		t.Fatalf("yield at five years (%d) is not greater than yield at the 180-day term (%d) — the "+
			"accrual now stops at the lock term. If that is the fix, delete this test and the "+
			"'unbounded' half of docs/stake-exposure-measured.md", atFiveYears, atTerm)
	}
	// At 20% APY over five years the mint is the whole principal.
	wantFive := int64(math.Floor(principal * APY180 * (5 * 365.0 / 365.0)))
	if atFiveYears != wantFive {
		t.Fatalf("five-year yield = %d, want %d (principal x APY x years) — the formula changed and "+
			"the report's arithmetic needs redoing", atFiveYears, wantFive)
	}
	t.Logf("MEASURED: 1 LENS staked at the %.0f%% tier mints %.4f LENS if unstaked at the 180-day "+
		"term, and %.4f LENS if left for five years. The lock is a MINIMUM HOLD, not a maximum "+
		"accrual window.", APY180*100, float64(atTerm)/1e6, float64(atFiveYears)/1e6)
}

// The per-term mint at each advertised tier — the numbers the report quotes.
func TestMeasured_ImpliedMintPerLockTerm(t *testing.T) {
	const principal = 1_000_000
	for _, tc := range []struct {
		days int
		apy  float64
	}{{30, APY30}, {90, APY90}, {180, APY180}} {
		got := computeYield(principal, tc.apy, time.Duration(tc.days)*24*time.Hour)
		want := int64(math.Floor(principal * tc.apy * (float64(tc.days) / 365.0)))
		if got != want {
			t.Fatalf("%dd@%.0f%%: yield = %d, want %d", tc.days, tc.apy*100, got, want)
		}
		t.Logf("MEASURED: %3dd @ %5.1f%% APY mints %6.4f%% of principal per lock term",
			tc.days, tc.apy*100, 100*float64(got)/principal)
	}
	// The tier table must match what the report says it is.
	if APY30 != 0.05 || APY90 != 0.12 || APY180 != 0.20 {
		t.Fatalf("the advertised APYs changed (%.4f/%.4f/%.4f) — every figure in "+
			"docs/stake-exposure-measured.md is now wrong and must be recomputed. ⚠ W6.3.3 says "+
			"DO NOT CHANGE THE APY; if it changed, that was somebody's decision and the report "+
			"needs to record it", APY30, APY90, APY180)
	}
}

// ⚠ THE YIELD IS A COUNTED MINT, which is the good news and the reason the dilution is at least
// VISIBLE. If it were ever dropped from the counted list, the same LENS would still be created and
// GetTotalSupply would stop seeing it — a strictly worse position than the one this item reports.
func TestMeasured_StakeYieldIsCountedInTotalSupply(t *testing.T) {
	found := false
	for _, ty := range mining.CountedSupplyTypes() {
		if ty == mining.TypeStakeYield {
			found = true
		}
	}
	if !found {
		t.Fatalf("%q is no longer in CountedSupplyTypes(). The yield is still minted, so it is now "+
			"real LENS in a wallet that GetTotalSupply cannot see — the exact defect the unstake "+
			"path's own comment says was found by #400's sweep and fixed", mining.TypeStakeYield)
	}
}
