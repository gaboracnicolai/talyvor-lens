package catalog

import (
	"encoding/json"
	"testing"
)

// THE ONE WAY INTO ResolveRates' "NO ZERO OUTCOME" INVARIANT IS THROUGH THE CATALOG'S OWN FRONT DOOR.
//
// resolve.go's package comment is absolute: ResolveRates "HAS NO ZERO OUTCOME ... every return is
// chargeable", and Rates is "never with zero values". Both statements are about the FALLBACK arm, which
// is carefully written to hold them (fallbackRates refuses to let a zero-priced entry anchor a bound and
// floors an empty catalog at emptyCatalogFloorPer1M). The EXACT arm has no such guard: it forwards
// whatever PriceDetailed reports for a model the catalog *knows*, and an operator override registers a
// model with no prices at all.
//
// That path is not hypothetical, and it is worse than the hole it sits next to: WarnUnpricedModel's own
// remediation text tells the operator to add the model via LENS_MODEL_CATALOG_OVERRIDES. Doing exactly
// that, with the price fields omitted or their JSON names mistyped, converts a request that was billed on
// a defensible floor — loudly, with a metric and a drift finding — into one billed ZERO, silently, and
// reported as `exact`.
//
// POPULATION, MEASURED rather than assumed (all 45 seeded models): NOT ONE has both rates zero. Three
// embedding models carry OutputPer1M == 0 with a positive input rate, which is correct — embeddings emit
// no output tokens — and is why the rule below is "input AND output", never "either".
func TestResolveRates_ZeroPricedOverrideIsNotExactAndIsNotFree(t *testing.T) {
	// A registry seeded with one real, fully priced model. Everything below is measured against it, so
	// the fallback arm has a genuine anchor and no assertion can pass by accident of an empty catalog.
	newReg := func() *Registry {
		return NewRegistry([]Model{{
			ID: "gpt-4o", Provider: "openai", InputPer1M: 2.50, OutputPer1M: 10.00,
			CachedInputPer1M: 1.25, CacheWritePer1M: 2.50,
		}})
	}

	// ── CONTROL 1: a model the catalog knows is exact and chargeable. ────────────────────────────
	// If this ever goes red the harness is measuring the wrong method, not a regression in the rule.
	if rates, prov := newReg().ResolveRates("gpt-4o", PurposeCharge); prov != ProvenanceExact || rates.InputPer1M <= 0 || rates.OutputPer1M <= 0 {
		t.Fatalf("CONTROL 1 (priced model): got %+v prov=%v, want exact and non-zero", rates, prov)
	}

	// ── CONTROL 2: a model the catalog does NOT know falls back, non-zero, and says so. ──────────
	// This is the behaviour the zero-priced case must join, and it proves the assertions below can
	// distinguish "chargeable + marked" from "free + unmarked".
	if rates, prov := newReg().ResolveRates("gpt-9-does-not-exist", PurposeCharge); prov != ProvenanceFallback || rates.InputPer1M <= 0 || rates.OutputPer1M <= 0 {
		t.Fatalf("CONTROL 2 (unknown model): got %+v prov=%v, want fallback and non-zero", rates, prov)
	}

	// ── THE MEASUREMENT: three ways an operator lands a priceless model in the catalog. ──────────
	// Each is decoded from JSON exactly as cmd/lens/main.go decodes LENS_MODEL_CATALOG_OVERRIDES
	// (json.Unmarshal into []Model, no DisallowUnknownFields), so the typo cases are the real thing
	// rather than a struct literal standing in for one.
	cases := []struct {
		name string
		raw  string
	}{
		{"price fields omitted entirely", `[{"id":"acme-1","provider":"openai"}]`},
		{"price field names mistyped (camelCase)", `[{"id":"acme-1","provider":"openai","inputPer1M":3.0,"outputPer1M":15.0}]`},
		{"price fields explicitly zero", `[{"id":"acme-1","provider":"openai","input_per_1m":0,"output_per_1m":0}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var overrides []Model
			if err := json.Unmarshal([]byte(tc.raw), &overrides); err != nil {
				t.Fatalf("override JSON did not decode (the operator would have seen a warning instead): %v", err)
			}
			r := newReg()
			r.LoadOverrides(overrides)

			// A charge on this model must be chargeable. A zero here is a served request that books
			// nothing: no bill, no floor, no trace.
			rates, prov := r.ResolveRates("acme-1", PurposeCharge)
			if rates.InputPer1M <= 0 || rates.OutputPer1M <= 0 ||
				rates.CachedInputPer1M <= 0 || rates.CacheWritePer1M <= 0 {
				t.Errorf("charge: ResolveRates returned a SPENDABLE ZERO %+v — resolve.go's invariant says every return is chargeable", rates)
			}
			// And it must not claim to be a published price. `exact` is what modelwatch skips on and
			// what keeps CostUSDResolved from warning: a wrong provenance turns a revenue hole invisible.
			if prov != ProvenanceFallback {
				t.Errorf("charge: provenance=%v, want fallback — the catalog does not know this model's price, it only knows its id", prov)
			}

			// The hold arm falls back in the opposite direction and must be non-zero too: a zero hold
			// reserves nothing, which is how a sub-budget ceiling leaks.
			holdRates, holdProv := r.ResolveRates("acme-1", PurposeHold)
			if holdRates.InputPer1M <= 0 || holdRates.OutputPer1M <= 0 {
				t.Errorf("hold: ResolveRates returned a SPENDABLE ZERO %+v", holdRates)
			}
			if holdProv != ProvenanceFallback {
				t.Errorf("hold: provenance=%v, want fallback", holdProv)
			}
			// The asymmetry that makes the fallback defensible must survive: a hold is never cheaper
			// than the charge it bounds.
			if holdRates.OutputPer1M < rates.OutputPer1M {
				t.Errorf("hold output rate %v is BELOW the charge rate %v — the hold no longer bounds the bill",
					holdRates.OutputPer1M, rates.OutputPer1M)
			}
		})
	}
}

// A model that is priced on ONE side only is a real, correct catalog state — the three seeded embedding
// models are exactly this — and it must keep its published price and its `exact` provenance. This is the
// blast-radius control for the rule above: it is the nearest neighbour to a zero-priced entry, and a rule
// written as "either rate is zero" instead of "both" would silently reprice every embeddings request in
// production. It reds if the rule ever widens.
func TestResolveRates_OutputOnlyZeroIsAPublishedPriceNotAnUnpricedModel(t *testing.T) {
	for _, id := range []string{"text-embedding-3-small", "text-embedding-3-large", "text-embedding-ada-002"} {
		in, _, _, out, ok := PriceDetailed(id)
		if !ok {
			t.Fatalf("%s: not in the seeded catalog — this control is measuring nothing", id)
		}
		if in <= 0 || out != 0 {
			t.Fatalf("%s: seeded as in=%v out=%v; this control assumes an embedding model (input>0, output==0)", id, in, out)
		}
		rates, prov := ResolveRates(id, PurposeCharge)
		if prov != ProvenanceExact {
			t.Errorf("%s: provenance=%v, want exact — its input rate IS published", id, prov)
		}
		if !approxEq(rates.InputPer1M, in) {
			t.Errorf("%s: input rate %v, want the published %v", id, rates.InputPer1M, in)
		}
		if rates.OutputPer1M != 0 {
			t.Errorf("%s: output rate %v, want 0 — embeddings emit no output tokens and must not be billed for any", id, rates.OutputPer1M)
		}
	}
}
