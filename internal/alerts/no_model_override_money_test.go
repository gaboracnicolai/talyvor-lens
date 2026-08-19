package alerts

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/metrics"
)

// THE UNNAMED MODEL IS A REAL REQUEST, AND IT MUST STAY ON THE LOUD ARM.
//
// proxy.extractPrompt returns req.Model with no non-empty check and HandleOpenAI serves a body that
// carries no `model` key at all with 200 — measured through the real entry point at 3fa1196, not
// inferred — so CostUSDResolved("") is a call this product makes, not a hypothetical. Today it takes
// the fallback arm: a defensible floor on the charge, the provider's most expensive model on the
// hold, an ERROR and a metric every time.
//
// An operator override document whose element names no model used to register a phantom entry keyed
// by the empty string, and the money path reads the catalog by exactly that key. The charge for the
// unnamed model then became `exact` at the phantom's rate: no ERROR, no metric, and modelwatch skips
// ProvenanceExact, so the drift detector went blind on it too. Measured at 3fa1196 on 1M in + 1M out,
// with `[{"model":"gpt-4o","input_per_1m":3.75,"output_per_1m":15.00}]` — the operator's typo for
// `id`, using the name the wire gives that field:
//
//	before the override   charge $0.02   fallback   hold $210.00  fallback   ERROR + metric
//	after  the override   charge $18.75  exact      hold $18.75   exact      silent
//
// …and gpt-4o, the model the operator was actually repricing, stayed on its seeded 2.50/10.00.
//
// catalog.DecodeOverrides now refuses that document. This asserts the consequence where it lands: on
// the function that produces the bill, over the package-level registry the proxy really reads.
//
// ⚠ IT ONLY EVER ASSERTS THE CLEAN DIRECTION, AND THAT IS FORCED RATHER THAN LAZY. Registry has put
// and no delete, so a phantom applied here could not be removed for the rest of the test binary and
// would silently reprice the unnamed model for every case that ran afterwards. The proof that the
// phantom DOES flip the provenance is therefore in internal/catalog, on a local registry
// (TestDecodeOverrides_APhantomEmptyIDEntryPricesTheUnnamedModel) — without it this file's green
// would be indistinguishable from a green over a rule that guards nothing.
func TestCostUSDResolved_TheUnnamedModelStaysOnTheFallbackArm(t *testing.T) {
	const (
		inTokens  = 1_000_000
		outTokens = 1_000_000
		unknown   = "acme-never-seen-8q4v"
	)

	// ── CONTROL 1: a priced model bills exact. If this drifts the harness is broken, not the rule.
	if usd, prov := CostUSDResolved("gpt-4o", catalog.PurposeCharge, inTokens, 0, 0, outTokens); prov != catalog.ProvenanceExact || usd <= 0 {
		t.Fatalf("CONTROL 1 (gpt-4o): $%v prov=%v, want a positive exact charge", usd, prov)
	}

	// ── CONTROL 2: the metric assertion below can move at all.
	beforeUnknown := testutil.CollectAndCount(metrics.UnpricedModelRequests)
	if _, prov := CostUSDResolved(unknown, catalog.PurposeCharge, inTokens, 0, 0, outTokens); prov != catalog.ProvenanceFallback {
		t.Fatalf("CONTROL 2 (unknown model): prov=%v, want fallback", prov)
	}
	if testutil.CollectAndCount(metrics.UnpricedModelRequests) <= beforeUnknown {
		t.Fatalf("CONTROL 2: the unpriced-model metric gained no series — the assertion below cannot fail")
	}

	// ── The operator's typo, decoded exactly as cmd/lens#applyCatalogOverrides decodes it.
	seededIn, _, _, seededOut, ok := catalog.PriceDetailed("gpt-4o")
	if !ok {
		t.Fatal("the shipped catalog has no gpt-4o — this test is measuring nothing")
	}
	overrides, err := catalog.DecodeOverrides([]byte(`[{"model":"gpt-4o","input_per_1m":3.75,"output_per_1m":15.00}]`))
	if err == nil {
		// Deliberately NOT applied: a phantom on the package-level registry is unremovable and would
		// poison every later case in this binary. The refusal is the assertion.
		t.Fatalf("DecodeOverrides accepted an element that names no model (%+v) — applying it would "+
			"register a phantom under the empty id and price the unnamed model `exact`", overrides)
	}

	// The catalog is untouched by the refusal, in both the directions that matter.
	if m, exists := catalog.Get(""); exists {
		t.Errorf("a phantom entry is registered under the empty id: %+v", m)
	}
	if in, _, _, out, k := catalog.PriceDetailed("gpt-4o"); !k || in != seededIn || out != seededOut {
		t.Errorf("gpt-4o = %v/%v ok=%v after a refused document, want its seeded %v/%v", in, out, k, seededIn, seededOut)
	}

	// ── THE MEASUREMENT: the unnamed model still bills loudly, in both arms.
	before := testutil.CollectAndCount(metrics.UnpricedModelRequests)
	usd, prov := CostUSDResolved("", catalog.PurposeCharge, inTokens, 0, 0, outTokens)
	after := testutil.CollectAndCount(metrics.UnpricedModelRequests)

	if usd <= 0 {
		t.Errorf("charge: a %d-input/%d-output request naming NO model billed $%v", inTokens, outTokens, usd)
	}
	if prov != catalog.ProvenanceFallback {
		t.Errorf("charge: provenance=%v, want fallback — `exact` is what suppresses WarnUnpricedModel and "+
			"what modelwatch skips on, so it blinds both ends at once", prov)
	}
	if after <= before {
		t.Errorf("charge: no unpriced-model metric series appeared (%d → %d)", before, after)
	}

	holdUSD, holdProv := CostUSDResolved("", catalog.PurposeHold, inTokens, 0, 0, outTokens)
	if holdProv != catalog.ProvenanceFallback {
		t.Errorf("hold: provenance=%v, want fallback", holdProv)
	}
	if holdUSD < usd {
		t.Errorf("hold $%v is below the charge $%v — the hold must bound the bill it settles", holdUSD, usd)
	}
}
