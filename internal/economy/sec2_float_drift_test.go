//go:build sec2_drift_red

// Run the drift proof explicitly:  go test -tags sec2_drift_red ./internal/economy/ -run TestSEC2 -v
// Gated behind a build tag so it does NOT wedge the default CI suite — these assert the EXACT-conservation
// contract the (founder-reviewed) float→integer migration must meet, and stay RED until that migration lands.

package economy

// SEC-2 (MONEY-PATH, FENCED): the LENS/LXC ledgers store balances in DOUBLE PRECISION. Float money is
// non-exact and non-associative, so real ledger arithmetic does not conserve value or recompute exactly.
// These tests reproduce the drift with the ACTUAL economy code (roundTo + TalyvorFeeRate + the real
// marketplace/conversion expressions). They are RED on purpose: they stay red until the float→integer
// migration lands (which is a founder-reviewed money-path change — NOT built here). Do not "fix" these
// by loosening the assertion; the exact-conservation assertion IS the contract the migration must meet.

import (
	"testing"
)

// A. Trade conservation leak. Real marketplace math (marketplace.go:339-344,372): a buyer's USD buys
// lensAmount = amountUSD/priceUSD; fee + netToBuyer are round-to-6dp; unsold is refunded raw. Tokens
// must be conserved: netToBuyer + fee + unsold == the listing amount. Under float they don't.
func TestSEC2_TradeConservation_Drifts(t *testing.T) {
	const listingAmount = 100.0
	const priceUSD = 3.0    // a price that does not divide evenly → 33.333... LENS
	const amountUSD = 100.0 // buy the whole listing

	lensAmount := amountUSD / priceUSD
	if lensAmount > listingAmount {
		lensAmount = listingAmount
	}
	fee := roundTo(lensAmount*TalyvorFeeRate, 6)
	netToBuyer := roundTo(lensAmount-fee, 6)
	unsold := listingAmount - lensAmount

	distributed := netToBuyer + fee + unsold
	if distributed != listingAmount {
		t.Errorf("SEC-2 float drift (trade conservation): netToBuyer(%.10f)+fee(%.10f)+unsold(%.10f) = %.12f, want EXACTLY %.1f — leak %.3e LENS created/destroyed per trade",
			netToBuyer, fee, unsold, distributed, listingAmount, distributed-listingAmount)
	}
}

// B. SUM(delta) reputation fold non-exactness. reputation.go:94 computes score = baseline + SUM(delta)
// into a `var sum float64`. Float summation is not exact, so the fold that gates task-access (< 0.35)
// and mint eligibility can land off the intended value. Demonstrated with the canonical non-exact sum
// (0.1+0.1+0.1 != 0.3 in float64) at the same fold type — under BIGINT-smallest-unit this is exact.
func TestSEC2_ReputationSumFold_NotExact(t *testing.T) {
	deltas := []float64{0.1, 0.2} // exact decimal sum 0.3
	var sum float64               // same type + fold as reputation.go:94's COALESCE(SUM(delta),0)
	for _, d := range deltas {
		sum += d
	}
	if sum != 0.3 {
		t.Errorf("SEC-2 float drift (reputation SUM(delta) fold): Σ(0.1,0.2) = %.17f, want exactly 0.3 — the float SUM(delta) that gates AccessFloor(0.35)/mint is non-exact (drift %.3e); under BIGINT smallest-unit it is exact",
			sum, sum-0.3)
	}
}
