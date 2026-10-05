package market

import "testing"

// B32.8 — Talyvor takes its fee from the first dollar: the seller keeps gross × (10,000 − take) ÷ 10,000,
// rounded down, so the fee rounds up and the two never sum to more than the sale.
func TestSellerShare_TakeFromTheFirstDollar(t *testing.T) {
	for _, c := range []struct {
		gross, takeBPS, want int64
	}{
		{1_000_000, 1500, 850_000}, // a $1.00 listing sale: 85% to the seller
		{1_000_000, 500, 950_000},  // a $1.00 payment to another company's agent: 95%
		{1, 1500, 0},               // one µUSD: the fee rounds up, never the share
		{7, 1500, 5},               // 5.95 → 5
		{5_000_000_000_000, 1500, 4_250_000_000_000}, // past the old US$1M line: still 85%
		{1 << 62, 1500, 3_919_933_115_663_279_718},   // ⌊2⁶² × 0.85⌋: no overflow on the multiply
		{1_000_000, 0, 1_000_000},                    // no take: all of it
	} {
		if got := SellerShare(c.gross, c.takeBPS); got != c.want {
			t.Errorf("SellerShare(gross %d, take %d bps) = %d, want %d", c.gross, c.takeBPS, got, c.want)
		}
	}
}
