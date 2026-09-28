package market

import "testing"

// B20.2 — a seller keeps all of what they earn up to US$1M lifetime, and 85% of whatever lies past it.
func TestSellerShare_FullUpToAMillionThen85Percent(t *testing.T) {
	const million = FullShareUpToUSDMicros
	for _, c := range []struct {
		lifetime, gross, want int64
	}{
		{0, 5_000, 5_000},                       // well under: all of it
		{million - 1_000, 5_000, 1_000 + 3_400}, // straddling: 1,000 in full, 85% of the other 4,000
		{million, 5_000, 4_250},                 // at the line: 85%
		{3 * million, 100, 85},
	} {
		if got := SellerShare(c.lifetime, c.gross); got != c.want {
			t.Errorf("SellerShare(lifetime %d, gross %d) = %d, want %d", c.lifetime, c.gross, got, c.want)
		}
	}
}
