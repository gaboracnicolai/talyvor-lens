// microunit.go — the SEC-2 integer money kernel.
//
// SEC-2 converted every CONSERVED token amount from float64 (DOUBLE PRECISION)
// to an int64 count of the token's SMALLEST UNIT ("micro-unit", µ):
//
//	1 LENS = 1_000_000 µLENS   (BIGINT)
//	1 LXC  = 1_000_000 µLXC    (BIGINT — the code's operative precision was
//	                            roundTo(_,6), i.e. 1e-6 LXC, so µLXC = 1e-6 LXC)
//
// Both tokens share the same 1e6 scale (MicroScale). Storing money as integers
// makes ledger arithmetic EXACT and associative: a credit + a debit of the same
// count cancel to the µLENS, so conservation holds with zero drift (the SEC-2
// drift proof). The float band-aid roundTo(v,6) — sprinkled at ~15 call sites to
// paper over IEEE-754 non-exactness — is deleted; integers don't need it.
//
// Tier-2 (rates: APY, conversion rate) and Tier-3 (USD backing/cost) values stay
// float64 for now. Where such a float feeds a CONSERVED (Tier-1) result, the
// RESULT is rounded to an integer µ-unit AT THE BOUNDARY with a HOUSE-FAVORING
// direction so rounding can never mint conserved value from nothing:
//
//	payouts / credits / mints  → round DOWN (MulFloor / FloatToMicroFloor)
//	charges / debits / slashes → round UP   (MulCeil  / FloatToMicroCeil)
//
// The dropped sub-unit remainder is thereby always retained by the protocol.
package mining

import "math"

// MicroScale is the number of micro-units in one whole token (LENS or LXC).
// 1 LENS = 1_000_000 µLENS; 1 LXC = 1_000_000 µLXC.
const MicroScale int64 = 1_000_000

// MulFloor returns floor(micro × factor) as an int64 µ-count. Use for a PAYOUT/
// MINT boundary where a Tier-2/3 float factor (a rate, a multiplier, a share)
// scales a conserved µ-amount: rounding DOWN keeps the protocol whole (never
// mints a sub-unit that wasn't earned). factor is assumed ≥ 0.
func MulFloor(micro int64, factor float64) int64 {
	return int64(math.Floor(float64(micro) * factor))
}

// MulCeil returns ceil(micro × factor) as an int64 µ-count. Use for a CHARGE/
// DEBIT/SLASH boundary where a float factor scales a conserved µ-amount:
// rounding UP keeps the protocol whole (the charge never under-collects a
// sub-unit). factor is assumed in [0,1] at the slash/fee sites so the result
// never exceeds the input. factor is assumed ≥ 0.
func MulCeil(micro int64, factor float64) int64 {
	return int64(math.Ceil(float64(micro) * factor))
}

// FloatToMicroFloor converts a whole-token float value (LENS/LXC/USD) to an
// int64 µ-count, rounding DOWN. Use at a PAYOUT/MINT boundary that starts from a
// float value (e.g. an anchor's LENS valuation, or amountUSD/priceUSD LENS).
func FloatToMicroFloor(v float64) int64 {
	return int64(math.Floor(v * float64(MicroScale)))
}

// FloatToMicroCeil converts a whole-token float value to an int64 µ-count,
// rounding UP. Use at a CHARGE/DEBIT boundary that starts from a float value.
func FloatToMicroCeil(v float64) int64 {
	return int64(math.Ceil(v * float64(MicroScale)))
}

// MicroToFloat converts an int64 µ-count back to a whole-token float. FOR
// DISPLAY / METRICS ONLY (Tier-2/3 observability) — never round-trip a conserved
// amount through this, it reintroduces float.
func MicroToFloat(micro int64) float64 {
	return float64(micro) / float64(MicroScale)
}
