package poolroyalty

import (
	"math"
	"testing"

	"github.com/talyvor/lens/internal/economy"
)

// TestRoyaltyNeverExceedsCharge is THE billing invariant, at the valuation point where the customer charge
// and the contributor mint meet. On a cross-tenant hit the consumer is charged avoided_COGS (V, dollars) and
// the contributor is minted the CostAnchor's Value from the SAME V. A royalty must NEVER exceed what the
// consumer paid for that request.
//
// The mint is denominated in LENS (Value = clamp01(Share) × V × LENSPerUSD), so the invariant is on its
// DOLLAR WORTH at the peg: mintUSD = Value × LXCUSDValue = clamp01(Share) × V. Since clamp01 ≤ 1,
// mintUSD ≤ V for EVERY V and EVERY Share — including a misconfigured Share>1 or a directly-built anchor
// that skipped NewMinter's clamp. The peg is a unit conversion, not an amplifier: it can never lift the
// dollar-worth ceiling above the charge.
func TestRoyaltyNeverExceedsCharge(t *testing.T) {
	for _, v := range []float64{0, 0.0001, 0.05, 1.0, 4.31, 1000} { // avoided_COGS = the customer charge (USD)
		for _, share := range []float64{0, 0.5, 1.0, 2.0, 100, -1, math.NaN()} { // incl. misconfig Share>1
			mintLENS := CostAnchor{Share: share}.Value(GainInput{AvoidedCOGSUSD: v})
			mintUSD := mintLENS * economy.LXCUSDValue // the mint's WORTH in dollars at the $0.10 peg
			charge := v                               // the consumer pays avoided_COGS (dollars)
			if mintLENS < 0 {
				t.Fatalf("negative royalty: share=%v v=%v mint=%v LENS", share, v, mintLENS)
			}
			if mintUSD > charge+1e-9 {
				t.Fatalf("ROYALTY EXCEEDS CHARGE: share=%v avoided=$%v → mint=%v LENS = $%v worth > charge=$%v", share, v, mintLENS, mintUSD, charge)
			}
		}
	}
}

// TestDefaultShareIsHalf pins the confirmed split: the contributor's HALF of a $10 avoided is $5 of value =
// 50 LENS at the $0.10 peg; Talyvor keeps the other $5. s=0.5.
func TestDefaultShareIsHalf(t *testing.T) {
	if DefaultRoyaltyShare != 0.5 {
		t.Fatalf("DefaultRoyaltyShare = %v, want 0.5", DefaultRoyaltyShare)
	}
	const v = 10.0
	mintLENS := CostAnchor{Share: DefaultRoyaltyShare}.Value(GainInput{AvoidedCOGSUSD: v})
	if mintLENS != 50.0 { // contributor half of $10 = $5 = 50 LENS at the peg
		t.Fatalf("mint at s=0.5 over avoided=$10 = %v LENS, want 50 (= $5 at the $0.10 peg)", mintLENS)
	}
	if mintUSD := mintLENS * economy.LXCUSDValue; mintUSD != 5.0 { // the contributor's half IN DOLLARS
		t.Fatalf("mint dollar-worth at s=0.5 = $%v, want $5 (Talyvor keeps the other $5)", mintUSD)
	}
}
