package alerts

import (
	"encoding/json"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/metrics"
)

// THE UNIT-LEVEL RULE IS WORTH NOTHING UNLESS IT REACHES THE BILL, SO THIS MEASURES THE BILL.
//
// CostUSDResolved is the entry point for everything that moves money (the reservation hold, the delivered
// charge, the pooled-hit valuation). Its doc comment's claim — "it cannot return zero for an unknown
// model, because catalog.ResolveRates has no unknown outcome" — was false for one shape of unknown: a
// model an operator had registered by NAME without a PRICE. Measured here through the real function, on a
// large request, in both directions:
//
//	BEFORE (measured at 0efb8c6, 1M uncached input + 1M output):
//	  zero-priced override  charge  $0.00  provenance=exact     unpriced-metric series 0 → 0   no ERROR
//	  zero-priced override  hold    $0.00  provenance=exact
//	  same request, model simply UNKNOWN  $0.02  fallback   metric 0 → 1   ERROR logged
//	  same request on gpt-4o             $12.50  exact
//
// The middle line is the one that matters: the operator's state BEFORE they followed WarnUnpricedModel's
// advice billed $0.02 loudly, and following it billed $0.00 silently. The remedy was worse than the defect.
func TestCostUSDResolved_ZeroPricedOverrideIsBilledAndMarkedLikeAnyUnknown(t *testing.T) {
	const (
		zeroPriced = "acme-priceless-4m8k"
		unknown    = "acme-never-seen-4m8k"
		inTokens   = 1_000_000
		outTokens  = 1_000_000
	)

	// Registered exactly as cmd/lens does it: json.Unmarshal of LENS_MODEL_CATALOG_OVERRIDES into
	// []catalog.Model, then LoadOverrides onto the default registry the money path reads.
	var overrides []catalog.Model
	if err := json.Unmarshal([]byte(`[{"id":"`+zeroPriced+`","provider":"openai"}]`), &overrides); err != nil {
		t.Fatalf("override JSON did not decode: %v", err)
	}
	catalog.LoadOverrides(overrides)
	if _, ok := catalog.Get(zeroPriced); !ok {
		t.Fatalf("%s did not register — this test would be measuring an unknown model, not an unpriced one", zeroPriced)
	}

	// ── CONTROL 1: a priced model bills its published price and stays exact. ────────────────────
	// 1M in at $2.50 + 1M out at $10.00 = $12.50. If this drifts, the harness is mispriced, not the rule.
	if usd, prov := CostUSDResolved("gpt-4o", catalog.PurposeCharge, inTokens, 0, 0, outTokens); prov != catalog.ProvenanceExact || usd <= 0 {
		t.Fatalf("CONTROL 1 (gpt-4o): $%v prov=%v, want a positive exact charge", usd, prov)
	}

	// ── CONTROL 2: an unknown model bills a floor, marks it, and increments the metric. ─────────
	// This is the behaviour the zero-priced model must match, and it proves the metric assertion below
	// can move at all.
	beforeUnknown := testutil.CollectAndCount(metrics.UnpricedModelRequests)
	unknownUSD, unknownProv := CostUSDResolved(unknown, catalog.PurposeCharge, inTokens, 0, 0, outTokens)
	afterUnknown := testutil.CollectAndCount(metrics.UnpricedModelRequests)
	if unknownProv != catalog.ProvenanceFallback || unknownUSD <= 0 {
		t.Fatalf("CONTROL 2 (unknown model): $%v prov=%v, want a positive fallback charge", unknownUSD, unknownProv)
	}
	if afterUnknown <= beforeUnknown {
		t.Fatalf("CONTROL 2: unpriced-model metric did not gain a series (%d → %d) — the metric assertion below cannot fail",
			beforeUnknown, afterUnknown)
	}

	// ── THE MEASUREMENT: the same request on the priceless override. ────────────────────────────
	before := testutil.CollectAndCount(metrics.UnpricedModelRequests)
	usd, prov := CostUSDResolved(zeroPriced, catalog.PurposeCharge, inTokens, 0, 0, outTokens)
	after := testutil.CollectAndCount(metrics.UnpricedModelRequests)

	if usd <= 0 {
		t.Errorf("charge: a %d-input/%d-output request on a registered-but-priceless model billed $%v — "+
			"served for free, with no spend row to reprice later", inTokens, outTokens, usd)
	}
	if prov != catalog.ProvenanceFallback {
		t.Errorf("charge: provenance=%v, want fallback — `exact` is what suppresses WarnUnpricedModel and "+
			"what modelwatch skips on, so it makes the hole invisible from both ends", prov)
	}
	if after <= before {
		t.Errorf("charge: no unpriced-model metric series appeared (%d → %d) — nothing graphable fires, "+
			"so the under-billing is undetectable without reading logs", before, after)
	}

	// The hold arm too: a zero hold reserves nothing, so the sub-budget ceiling is never consulted and
	// the settle has nothing to charge against — the exact failure reserveEstimateLXC's comment describes.
	holdUSD, holdProv := CostUSDResolved(zeroPriced, catalog.PurposeHold, inTokens, 0, 0, outTokens)
	if holdUSD <= 0 {
		t.Errorf("hold: reserved $%v on a priceless model — a hold of nothing leaks the ceiling", holdUSD)
	}
	if holdProv != catalog.ProvenanceFallback {
		t.Errorf("hold: provenance=%v, want fallback", holdProv)
	}
	if holdUSD < usd {
		t.Errorf("hold $%v is below the charge $%v — the hold must bound the bill it settles", holdUSD, usd)
	}
}
