package economy

import (
	"math/rand"
	"testing"
)

// B32.11 — the platform fee rounds up to the µLXC, and a settle clamped to spendWithin never charges its
// spend plus fee above the hold.
func TestB3211_PlatformFeeRoundsUpToTheMicroLXC(t *testing.T) {
	for _, tc := range []struct{ amount, bps, want int64 }{
		{10_000_000, 300, 300_000}, // Team: 10 LXC carries 0.3 LXC
		{10_000_000, 550, 550_000}, // Free
		{10_000_000, 100, 100_000}, // Business
		{1, 300, 1},                // a sub-µLXC fee is a whole µLXC
		{260_001, 300, 7_801},
		{10_000_000, 0, 0},
		{0, 300, 0},
		{1 << 60, 10_000, 1 << 60}, // no overflow at any amount a call can cost
	} {
		if got := PlatformFee(tc.amount, tc.bps); got != tc.want {
			t.Errorf("PlatformFee(%d, %d) = %d, want %d", tc.amount, tc.bps, got, tc.want)
		}
	}
}

func TestB3211_SpendWithinIsTheLargestSpendWhoseFeeFitsTheHold(t *testing.T) {
	r := rand.New(rand.NewSource(3211))
	for i := 0; i < 100_000; i++ {
		gross, bps := r.Int63n(1_000_000_000_000), r.Int63n(10_001)
		s := spendWithin(gross, bps)
		if s+PlatformFee(s, bps) > gross {
			t.Fatalf("spendWithin(%d, %d) = %d: with its fee %d it is over the hold", gross, bps, s, PlatformFee(s, bps))
		}
		if s+1+PlatformFee(s+1, bps) <= gross {
			t.Fatalf("spendWithin(%d, %d) = %d, but %d and its fee also fit", gross, bps, s, s+1)
		}
	}
	if got := spendWithin(10_300_000, 300); got != 10_000_000 {
		t.Errorf("a 10.3 LXC hold at 3%% settles at most %d, want 10,000,000", got)
	}
}

func TestB3211_FeeLabelNamesTheRate(t *testing.T) {
	for bps, want := range map[int64]string{300: "Platform fee 3%", 550: "Platform fee 5.5%", 100: "Platform fee 1%", 15: "Platform fee 0.15%"} {
		if got := FeeLabel(bps); got != want {
			t.Errorf("FeeLabel(%d) = %q, want %q", bps, got, want)
		}
	}
}
