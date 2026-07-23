package poolroyalty

import "math"

// Proof-of-Improvement rail, piece 1 — the pluggable reward ANCHOR.
//
// An Anchor answers exactly one question: "what is this measured improvement worth, in LENS?" It is
// pure VALUATION — it NEVER touches the ledger, opens no tx, and reaches no mint guard. The minter
// feeds the anchor's output as the mint `amount` into the SAME held-ledger chokepoint (verifyEarn +
// reputation bond + 1000-LENS/24h rate cap), so the U6 guarantees are unchanged regardless of which
// anchor priced the gain. An anchor returns a NON-NEGATIVE amount; 0 means "mint nothing."
//
// Two anchors today:
//   - CostAnchor (the proof-of-savings #2 anchor): value = Share × avoided_COGS$ — a DOLLAR anchor,
//     anchored to the catalog price table. This is the existing, default, byte-identical path.
//   - HeldBenchmarkAnchor (the #10 eval-pool pattern): value = RatePerPoint × clamp01(score) — a score
//     vs verifier-HELD ground truth. BUILT + unit-tested but wired to nothing live this PR (no
//     reachable selection, no new mint surface).
type Anchor interface {
	// Value returns the LENS amount a measured improvement is worth (≥ 0; 0 ⇒ mint nothing).
	Value(g GainInput) float64
	// Kind labels the anchor for audit ("cost" | "held_benchmark"). Recorded in mint metadata ONLY
	// when a non-cost anchor mints (the cost path's metadata stays byte-identical to #2).
	Kind() string
}

// GainInput is the union of valuation inputs; each anchor reads only the field it needs. The minter
// fills the field relevant to its source (cache/distill avoided-COGS today; a held score in future).
type GainInput struct {
	AvoidedCOGSUSD float64 // cost anchor: what the requester avoided (the existing #2 number)
	HeldScore      float64 // held-benchmark anchor: a quality score ∈ [0,1] vs verifier-held ground truth
}

// CostAnchor is the proof-of-savings anchor: Value = clamp01(Share) × AvoidedCOGSUSD.
//
// THE BILLING INVARIANT, structural: the customer is charged avoided_COGS for a cross-tenant hit, and the
// contributor is minted this anchor's Value from the SAME avoided_COGS. Clamping Share to [0,1] HERE — at
// the valuation point, not only in NewMinter — guarantees Value ≤ AvoidedCOGSUSD unconditionally, so a
// royalty can NEVER exceed what the consumer paid for that request, even for a CostAnchor built directly
// with a misconfigured Share>1 (bypassing NewMinter's clamp). One V, two derivations that cannot drift.
type CostAnchor struct{ Share float64 }

func (a CostAnchor) Value(g GainInput) float64 {
	s := a.Share
	if math.IsNaN(s) || s < 0 {
		s = 0
	} else if s > 1 {
		s = 1 // royalty is a SHARE of avoided_COGS — never more than the consumer was charged
	}
	return s * g.AvoidedCOGSUSD
}
func (a CostAnchor) Kind() string { return "cost" }

// HeldBenchmarkAnchor prices a held-benchmark improvement: Value = RatePerPoint × clamp01(HeldScore),
// where HeldScore is measured against verifier-private ground truth (the #10 eval pool) and supplied
// to the anchor as a GainInput — the anchor itself reads no DB and writes nothing, so a mint paid on
// it can NEVER feed back into the score it is paid on (NO-LOOP by construction).
//
// RatePerPoint is REQUIRED — there is no default. NewHeldBenchmarkAnchor rejects a non-positive rate
// so an unset rate can never silently mint (loud absence, not a papered-over zero). This anchor is
// constructed only in tests this PR; the live caller (a future proof-of-eval-contribution mint) must
// supply the rate explicitly.
type HeldBenchmarkAnchor struct{ ratePerPoint float64 }

// NewHeldBenchmarkAnchor requires a positive LENS-per-quality-point rate; ok=false makes a missing
// rate loud at the construction site rather than minting 0 (or worse) silently.
func NewHeldBenchmarkAnchor(ratePerPoint float64) (HeldBenchmarkAnchor, bool) {
	if math.IsNaN(ratePerPoint) || math.IsInf(ratePerPoint, 0) || ratePerPoint <= 0 {
		return HeldBenchmarkAnchor{}, false
	}
	return HeldBenchmarkAnchor{ratePerPoint: ratePerPoint}, true
}

func (a HeldBenchmarkAnchor) Value(g GainInput) float64 {
	s := g.HeldScore
	if math.IsNaN(s) || s <= 0 {
		return 0
	}
	if s > 1 {
		s = 1 // clamp01: a score is a bounded quality fraction; never amplify past full
	}
	return a.ratePerPoint * s
}

func (a HeldBenchmarkAnchor) Kind() string { return "held_benchmark" }
