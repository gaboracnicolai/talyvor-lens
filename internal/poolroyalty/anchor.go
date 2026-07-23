package poolroyalty

import (
	"math"

	// economy is imported ONLY for the published LENS/USD peg constant
	// (economy.LENSPerUSD). It is a fixed pure constant, not a score this anchor
	// prices — so the NO-LOOP / pure-valuation posture holds (TestAnchor_NoLedgerNoMint_ImportGuard
	// forbids ledger/DB/score-producer imports; a currency peg is none of those).
	"github.com/talyvor/lens/internal/economy"
)

// Proof-of-Improvement rail, piece 1 — the pluggable reward ANCHOR.
//
// An Anchor answers exactly one question: "what is this measured improvement worth, in LENS?" It is
// pure VALUATION — it NEVER touches the ledger, opens no tx, and reaches no mint guard. The minter
// feeds the anchor's output as the mint `amount` into the SAME held-ledger chokepoint (verifyEarn +
// reputation bond + 1000-LENS/24h rate cap), so the U6 guarantees are unchanged regardless of which
// anchor priced the gain. An anchor returns a NON-NEGATIVE amount; 0 means "mint nothing."
//
// UNITS ARE LENS. Every anchor returns LENS, because the minter converts the result to µLENS with
// FloatToMicroFloor and credits it verbatim. An anchor whose SOURCE is dollars (CostAnchor) MUST
// therefore convert to LENS at the published peg — returning raw dollars silently underpays 10× (the
// pre-peg bug: a $5-of-value mint credited 5 LENS = $0.50 at the $0.10 peg instead of 50 LENS).
//
// Two anchors today:
//   - CostAnchor (the proof-of-savings #2 anchor): value = Share × avoided_COGS$ × LENSPerUSD — a
//     DOLLAR source converted to LENS at the peg, anchored to the catalog price table.
//   - HeldBenchmarkAnchor (the #10 eval-pool pattern): value = RatePerPoint × clamp01(score). Its rate
//     is ALREADY denominated in LENS-per-point (config: "LENS per discrimination-point"), so it needs
//     NO peg conversion — it is not a dollar source. BUILT + unit-tested; wired to the four P-o-I mints.
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

// CostAnchor is the proof-of-savings anchor: Value = clamp01(Share) × AvoidedCOGSUSD × LENSPerUSD —
// the contributor's share of the avoided COGS, expressed IN VALUE (LENS) at the published $0.10 peg
// (economy.LENSPerUSD = 10 LENS/$). So a $10 avoided at s=0.5 is $5 of value = 50 LENS (not 5 — that
// raw-dollar reading was the 10× underpay).
//
// THE BILLING INVARIANT, structural, SURVIVES THE PEG: the customer is charged avoided_COGS for a
// cross-tenant hit, and the contributor is minted this anchor's Value from the SAME avoided_COGS.
// clamp01(Share) HERE — at the valuation point, not only in NewMinter — bounds the royalty's DOLLAR
// worth (Value × LXCUSDValue = clamp01(Share) × AvoidedCOGSUSD) to ≤ AvoidedCOGSUSD unconditionally,
// so a royalty can NEVER exceed what the consumer paid — even for a CostAnchor built directly with a
// misconfigured Share>1 (bypassing NewMinter's clamp). The peg is a UNIT conversion (dollars→LENS),
// not an amplifier: it scales the VALUE and its dollar-worth ceiling identically. One V, two
// derivations that cannot drift.
type CostAnchor struct{ Share float64 }

// Value clamps Share to [0,1] AND converts the dollar avoided-COGS to LENS at the peg (economy.LENSPerUSD
// = 10 LENS/$). BOTH happen HERE, at the single valuation point where the dollar→LENS unit crosses over —
// so the mint amount, the held credit, and the margin view all agree on 1 LENS = $0.10, and Value's
// dollar-worth stays ≤ avoided_COGS for any Share (the billing invariant above).
func (a CostAnchor) Value(g GainInput) float64 {
	s := a.Share
	if math.IsNaN(s) || s < 0 {
		s = 0
	} else if s > 1 {
		s = 1 // royalty is a SHARE of avoided_COGS — never more than the consumer was charged
	}
	return s * g.AvoidedCOGSUSD * economy.LENSPerUSD
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
