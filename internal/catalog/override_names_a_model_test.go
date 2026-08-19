package catalog

import (
	"strings"
	"testing"
)

// AN OVERRIDE ELEMENT THAT NAMES NO MODEL IS NOT AN OVERRIDE.
//
// LENS_MODEL_CATALOG_OVERRIDES is a JSON array of Model, and the model it speaks about is the `id`
// field. An element that carries no usable id — because the key was omitted, or because it was
// written as `model` or `name`, which is what the rest of this product calls that field on the wire —
// used to decode to a Model with ID "" and be handed to put, which keys byID by that empty string.
//
// Two things happened at once, and neither of them was said out loud:
//
//  1. THE REPRICE THE OPERATOR WROTE DID NOT HAPPEN. `[{"model":"gpt-4o","input_per_1m":3.75,
//     "output_per_1m":15.00}]` left gpt-4o on its seeded 2.50/10.00, while cmd/lens logged
//     "catalog: applied model overrides count=1".
//
//  2. A PHANTOM ENTRY ENTERED THE CATALOG UNDER THE EMPTY ID, AND THE MONEY PATH READS IT.
//     TestDecodeOverrides_APhantomEmptyIDEntryPricesTheUnnamedModel below measures what that costs.
//
// ⚠ AND THE EMPTY MODEL ID IS REACHABLE, MEASURED THROUGH THE PROXY'S REAL ENTRY POINT RATHER THAN
// ASSUMED: extractPrompt returns req.Model with no non-empty check, and HandleOpenAI serves a body
// with no `model` key at all with 200. So `CostUSDResolved("")` is a live call, not a hypothetical
// one — at 3fa1196 it billed charge $0.02 / hold $210.00 on the LOUD fallback arm, and with the
// phantom registered it billed $18.75 on both, `exact`, with no ERROR and no metric.
//
// The rule is therefore stated where the document is read: DecodeOverrides refuses an element that
// names no model, and names the element it refused.
//
// ⚠ THE WHOLE DOCUMENT IS REFUSED, NOT JUST THE ELEMENT, and that is a choice rather than an
// accident. cmd/lens#applyCatalogOverrides already applies NOTHING when the document does not parse,
// so "Lens could not read this document" has one meaning and one outcome. The alternative — apply the
// elements that do name a model and skip the rest — keeps more pricing but leaves a half-applied
// document the operator has to reason about. Refusing costs nothing on the money axis: every model
// the document meant to price falls to ResolveRates' fallback arm, which is non-zero by construction,
// defensible, and screams on every request (WarnUnpricedModel: ERROR + metric + a modelwatch drift
// finding). Loud-and-unpriced beats silent-and-wrong, which is this package's whole posture.
func newNamedReg() *Registry {
	return NewRegistry([]Model{{
		ID: "gpt-4o", Provider: "openai", DisplayName: "GPT-4o",
		InputPer1M: 2.50, OutputPer1M: 10.00, CachedInputPer1M: 1.25, CacheWritePer1M: 2.50,
		Capabilities:  Capabilities{Vision: true},
		ContextTokens: 128000, MaxOutput: 16384,
	}})
}

func TestDecodeOverrides_RefusesAnElementThatNamesNoModel(t *testing.T) {
	// The operator shapes, each one a real way to write this document wrong. Every one of them
	// decoded to ID "" before this rule existed — verified case by case, not assumed from one.
	cases := []struct {
		name string
		doc  string
	}{
		{"the id key is omitted entirely", `[{"provider":"openai","input_per_1m":0.001,"output_per_1m":0.002}]`},
		{"written as `model`, which is what the wire calls it", `[{"model":"gpt-4o","input_per_1m":3.75,"output_per_1m":15.00}]`},
		{"written as `name`", `[{"name":"gpt-4o","input_per_1m":3.75}]`},
		{"an explicitly empty id", `[{"id":"","input_per_1m":3.75,"output_per_1m":15.00}]`},
		{"an id that is only whitespace", `[{"id":"   ","input_per_1m":3.75,"output_per_1m":15.00}]`},
		{"an empty object", `[{}]`},
		{"a good element and a nameless one", `[{"id":"gpt-4o","input_per_1m":3.75},{"model":"gpt-4o-mini"}]`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newNamedReg()

			// CONTROL: the seeded price this document claims to change is really there. Without it a
			// registry that never held gpt-4o would score identically to one that kept it.
			if in, _, _, out, ok := r.PriceDetailed("gpt-4o"); !ok || in != 2.50 || out != 10.00 {
				t.Fatalf("CONTROL: seeded gpt-4o = %v/%v ok=%v — this case is measuring nothing", in, out, ok)
			}
			before := len(r.All())

			out, err := r.DecodeOverrides([]byte(c.doc))
			if err == nil {
				t.Fatalf("DecodeOverrides ACCEPTED an element that names no model and returned %+v — "+
					"it registers under the empty id and the operator is told nothing", out)
			}
			if !strings.Contains(err.Error(), "id") {
				t.Errorf("error %q does not mention the field that is missing — an operator reading the "+
					"boot log has to guess which of their elements is wrong", err)
			}

			// A refused document changes NOTHING. Decode is read-only on the registry; the caller
			// never reaches LoadOverrides because it returns on the error.
			if got := len(r.All()); got != before {
				t.Errorf("catalog went from %d entries to %d on a REFUSED document", before, got)
			}
			if m, ok := r.Get(""); ok {
				t.Errorf("a phantom entry registered under the empty id: %+v", m)
			}
			if in, _, _, out, ok := r.PriceDetailed("gpt-4o"); !ok || in != 2.50 || out != 10.00 {
				t.Errorf("gpt-4o = %v/%v ok=%v after a refused document, want its seeded 2.50/10.00", in, out, ok)
			}
		})
	}
}

// THE MUST-STAY-GREEN COMPANION. A rule that refuses everything is not a rule, and this is the only
// thing standing between "refuses an element that names no model" and "refuses operator overrides".
func TestDecodeOverrides_StillAcceptsEveryDocumentThatNamesItsModel(t *testing.T) {
	r := newNamedReg()

	out, err := r.DecodeOverrides([]byte(`[
		{"id":"gpt-4o","input_per_1m":3.75,"output_per_1m":15.00},
		{"id":"acme-new-8q4v","provider":"openai","input_per_1m":1.00,"output_per_1m":2.00}
	]`))
	if err != nil {
		t.Fatalf("a well-formed document was refused: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("decoded %d elements, want 2", len(out))
	}
	r.LoadOverrides(out)

	// The reprice really lands (a green that came from the override doing nothing proves nothing) …
	if in, _, _, o, ok := r.PriceDetailed("gpt-4o"); !ok || in != 3.75 || o != 15.00 {
		t.Errorf("reprice: gpt-4o = %v/%v ok=%v, want 3.75/15.00", in, o, ok)
	}
	// … and #450's rule still holds on top of it: a price states a price, it withdraws nothing.
	if m, _ := r.Get("gpt-4o"); !m.Capabilities.Vision || m.Provider != "openai" {
		t.Errorf("reprice withdrew a fact: %+v", m)
	}
	// … and a brand-new model still registers.
	if in, _, _, o, ok := r.PriceDetailed("acme-new-8q4v"); !ok || in != 1.00 || o != 2.00 {
		t.Errorf("new model: %v/%v ok=%v, want 1.00/2.00", in, o, ok)
	}
}

// WHAT THE REFUSAL IS WORTH, MEASURED ON THE MONEY PATH RATHER THAN ARGUED.
//
// This applies the phantom DIRECTLY through LoadOverrides — the batch applier, which is deliberately
// not the validator — because the number it produces is the reason the validation exists. Without
// this case "DecodeOverrides returns an error" is a claim about a function, not about a bill.
//
// ⚠ IT RUNS ON A LOCAL REGISTRY, AND NOT ONLY FOR ISOLATION: the registry has put and no delete, so a
// phantom applied to the package-level registry could not be removed again for the rest of the
// process. That is itself worth knowing — an operator who ships this typo cannot un-ship it without a
// restart — and it is why the global-registry half of this measurement lives in internal/alerts and
// only ever asserts the direction that leaves the catalog clean.
func TestDecodeOverrides_APhantomEmptyIDEntryPricesTheUnnamedModel(t *testing.T) {
	const inTok, outTok = 1_000_000, 1_000_000

	// ⚠ TWO PRICED MODELS, DELIBERATELY. The fallback arm ranks a HOLD to the most expensive known
	// model and a CHARGE to the cheapest; on a one-model registry those are the same number and the
	// two opposite failures below would be indistinguishable.
	twoModel := func() *Registry {
		return NewRegistry([]Model{
			{ID: "gpt-4o", Provider: "openai", InputPer1M: 2.50, OutputPer1M: 10.00},
			{ID: "gpt-premium-8q4v", Provider: "openai", InputPer1M: 15.00, OutputPer1M: 75.00},
		})
	}

	clean := twoModel()
	chargeBefore, provBefore := clean.ResolveRates("", PurposeCharge)
	holdBefore, holdProvBefore := clean.ResolveRates("", PurposeHold)

	// CONTROL: with no phantom, the unnamed model is an unknown — the loud arm, by construction.
	if provBefore != ProvenanceFallback || holdProvBefore != ProvenanceFallback {
		t.Fatalf("CONTROL: ResolveRates(\"\") is charge=%v hold=%v on a clean registry, want fallback both — "+
			"this case cannot show a flip", provBefore, holdProvBefore)
	}

	if costOf(holdBefore, inTok, outTok) <= costOf(chargeBefore, inTok, outTok) {
		t.Fatalf("CONTROL: the fallback hold ($%v) is not above the fallback charge ($%v) — the two arms "+
			"are not distinguishable on this registry", costOf(holdBefore, inTok, outTok), costOf(chargeBefore, inTok, outTok))
	}

	dirty := twoModel()
	dirty.LoadOverrides([]Model{{ID: "", InputPer1M: 3.75, OutputPer1M: 15.00}})
	chargeAfter, provAfter := dirty.ResolveRates("", PurposeCharge)
	holdAfter, holdProvAfter := dirty.ResolveRates("", PurposeHold)

	if provAfter != ProvenanceExact || holdProvAfter != ProvenanceExact {
		t.Fatalf("the phantom did not change the provenance (charge=%v hold=%v) — either put stopped "+
			"keying on the empty id or ResolveRates stopped reading it, and DecodeOverrides' refusal is "+
			"now guarding nothing. Re-measure before deleting the rule.", provAfter, holdProvAfter)
	}

	// The direction of both errors, stated as numbers so a future reader does not have to re-derive
	// them: the charge is an OVER-bill on a rate that names no model, and the hold — which exists to
	// bound the bill it settles — collapses to that same number.
	cBefore := costOf(chargeBefore, inTok, outTok)
	cAfter := costOf(chargeAfter, inTok, outTok)
	hBefore := costOf(holdBefore, inTok, outTok)
	hAfter := costOf(holdAfter, inTok, outTok)
	if cAfter <= cBefore {
		t.Errorf("phantom charge $%v is not above the defensible floor $%v — the over-bill this rule "+
			"prevents did not reproduce", cAfter, cBefore)
	}
	if hAfter >= hBefore {
		t.Errorf("phantom hold $%v is not below the fallback hold $%v — the ceiling collapse did not "+
			"reproduce", hAfter, hBefore)
	}
	t.Logf("MEASURED on %d in + %d out: charge $%v (fallback) → $%v (exact); hold $%v (fallback) → $%v (exact)",
		inTok, outTok, cBefore, cAfter, hBefore, hAfter)
}

func costOf(r Rates, in, out int) float64 {
	return (float64(in)*r.InputPer1M + float64(out)*r.OutputPer1M) / 1_000_000
}
