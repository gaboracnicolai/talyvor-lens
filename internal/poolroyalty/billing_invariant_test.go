package poolroyalty

import (
	"math"
	"testing"
)

// TestRoyaltyNeverExceedsCharge is THE billing invariant, at the valuation point where the customer charge
// and the contributor mint meet. On a cross-tenant hit the consumer is charged avoided_COGS (V) and the
// contributor is minted the CostAnchor's Value from the SAME V. A royalty must NEVER exceed what the consumer
// paid for that request. Since Value = clamp01(Share) × V and clamp01 ≤ 1, Value ≤ V for EVERY V and EVERY
// Share — including a misconfigured Share>1 or a directly-built anchor that skipped NewMinter's clamp.
func TestRoyaltyNeverExceedsCharge(t *testing.T) {
	for _, v := range []float64{0, 0.0001, 0.05, 1.0, 4.31, 1000} { // avoided_COGS = the customer charge (USD)
		for _, share := range []float64{0, 0.5, 1.0, 2.0, 100, -1, math.NaN()} { // incl. misconfig
			mint := CostAnchor{Share: share}.Value(GainInput{AvoidedCOGSUSD: v})
			charge := v // the consumer pays avoided_COGS
			if mint < 0 {
				t.Fatalf("negative royalty: share=%v v=%v mint=%v", share, v, mint)
			}
			if mint > charge+1e-9 {
				t.Fatalf("ROYALTY EXCEEDS CHARGE: share=%v avoided=%v → mint=%v > charge=%v", share, v, mint, charge)
			}
		}
	}
}

// TestDefaultShareIsHalf pins the confirmed split: contributor s·V, Talyvor keeps (1−s)·V, s=0.5.
func TestDefaultShareIsHalf(t *testing.T) {
	if DefaultRoyaltyShare != 0.5 {
		t.Fatalf("DefaultRoyaltyShare = %v, want 0.5", DefaultRoyaltyShare)
	}
	const v = 10.0
	mint := CostAnchor{Share: DefaultRoyaltyShare}.Value(GainInput{AvoidedCOGSUSD: v})
	if mint != 5.0 { // contributor half
		t.Fatalf("mint at s=0.5 over avoided=10 = %v, want 5 (Talyvor keeps the other 5)", mint)
	}
}
