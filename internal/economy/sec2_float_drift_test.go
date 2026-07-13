// SEC-2 conservation proof. This test began life on the sec2-ledger-float-recon
// branch, build-tagged and RED: it PROVED the float ledger did not conserve value
// (a ~3.33e-7 LENS leak per trade). After the float→integer-µLENS migration landed,
// the SAME trade cycle now conserves EXACTLY (0 drift). The build tag is gone so
// this guards conservation as part of the standing economy suite — the RED→GREEN
// flip IS the proof that SEC-2 fixed the money path.
//
//	Run:  go test ./internal/economy/ -run TestSEC2 -v

package economy

import "testing"

// A. Trade conservation. The real marketplace split (tradeSplit, exercised by
// ExecuteTrade) must conserve tokens EXACTLY: netToBuyer + fee + unsold ==
// listingAmount, to the µLENS, for a price that does NOT divide evenly. Under the
// old float ledger this drifted by ~3.33e-7 LENS/trade; under integer µLENS it is
// exact.
func TestSEC2_TradeConservation_Exact(t *testing.T) {
	const listingAmount int64 = 100_000_000 // 100 LENS in µLENS
	const priceUSD = 3.0                    // does not divide evenly → 33.333… LENS
	const amountUSD = 100.0                 // buy the whole listing

	lensAmount, fee, netToBuyer, unsold := tradeSplit(listingAmount, priceUSD, amountUSD)

	// The buyer's LENS is split with no remainder…
	if netToBuyer+fee != lensAmount {
		t.Errorf("SEC-2 split leak: netToBuyer(%d)+fee(%d) = %d, want lensAmount %d µLENS",
			netToBuyer, fee, netToBuyer+fee, lensAmount)
	}
	// …and the whole listing is conserved to the µLENS.
	distributed := netToBuyer + fee + unsold
	if distributed != listingAmount {
		t.Errorf("SEC-2 conservation leak: netToBuyer(%d)+fee(%d)+unsold(%d) = %d µLENS, want EXACTLY %d — drift %d µLENS",
			netToBuyer, fee, unsold, distributed, listingAmount, distributed-listingAmount)
	}
	// House-favoring rounding: the fee (a charge) rounds UP, so the buyer is never
	// over-credited. For 33_333_333 µLENS × 5% the exact cut is 1_666_666.65 µLENS,
	// so fee ceils to 1_666_667 and netToBuyer is the exact remainder 31_666_666.
	if fee != 1_666_667 || netToBuyer != 31_666_666 || unsold != 66_666_667 {
		t.Errorf("SEC-2 split unexpected: lensAmount=%d fee=%d net=%d unsold=%d",
			lensAmount, fee, netToBuyer, unsold)
	}
}

// B. Repeated-trade conservation. Conservation must hold over a CYCLE of trades at
// awkward prices (the property float summation broke): the sum of all splits equals
// the sum of all listings, exactly, with zero accumulated drift.
func TestSEC2_TradeConservation_NoAccumulatedDrift(t *testing.T) {
	prices := []float64{3.0, 7.0, 11.0, 13.0, 0.3, 1.7, 9.9}
	var totalListed, totalDistributed int64
	for _, p := range prices {
		const listing int64 = 100_000_000 // 100 LENS
		// Buy an awkward USD amount so lensAmount rarely divides evenly.
		_, fee, net, unsold := tradeSplit(listing, p, 42.0)
		totalListed += listing
		totalDistributed += net + fee + unsold
	}
	if totalListed != totalDistributed {
		t.Errorf("SEC-2 accumulated drift over %d trades: listed %d µLENS, distributed %d µLENS — drift %d µLENS (want 0)",
			len(prices), totalListed, totalDistributed, totalDistributed-totalListed)
	}
}
