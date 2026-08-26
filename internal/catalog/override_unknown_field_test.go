package catalog

import (
	"strings"
	"testing"
)

// A MISTYPED FIELD IN AN OVERRIDE DOCUMENT USED TO BILL THE OUTPUT TOKENS AT ZERO, SILENTLY,
// AND REPORT THE PRICE AS `exact`.
//
// LENS_MODEL_CATALOG_OVERRIDES is a JSON array of Model. #451 closed the half of this where an
// element names no model. This is the other half, and it is the one resolve.go's own comment on
// `unpriced` already described in prose without anything enforcing it:
//
//	"That JSON is decoded without DisallowUnknownFields, so omitting the price fields — or
//	 mistyping input_per_1m as inputPer1M — registers the model with 0.00 and no error."
//
// ⚠ WHY `unpriced` DOES NOT CATCH IT, AND CANNOT BE WIDENED TO. `unpriced(in, out)` is
// `in <= 0 && out <= 0` — BOTH, never EITHER — and that is load-bearing: three seeded embeddings
// models carry OutputPer1M == 0 as their PUBLISHED price, so an either-rate rule would reprice
// every embeddings request in production onto a fallback. So the both-zero document is already
// caught and falls to the loud fallback arm. THE ONE-ZERO DOCUMENT IS NOT, AND IS INDISTINGUISHABLE
// FROM AN EMBEDDINGS MODEL BY ITS RATES ALONE. It has to be caught where the document is READ.
//
// ⚠ MEASURED AT abdcbd5 BEFORE THIS RULE EXISTED, through DecodeOverrides + LoadOverrides +
// ResolveRates, never inferred from the source:
//
//	[{"id":"gpt-4o","input_per_1m":3.75,"outputPer1M":15.00}]
//	  -> accepted, err == nil
//	  -> OutputPer1M 0.00
//	  -> ResolveRates(charge) = 3.75 / 0.00, provenance EXACT
//	  -> ResolveRates(hold)   = 3.75 / 0.00, provenance EXACT
//
// The operator wrote a reprice to $15.00 per 1M output. Every output token then billed at ZERO,
// on the arm that says the catalog knows this price. No error, no metric, no drift finding —
// `modelwatch.Check` skips ProvenanceExact, so the drift detector goes blind on the same byte.
// This is #451's shape one door along: silent, and in the direction that loses money.
//
// ⚠ AND NOT EVERY MISSPELLING IS ONE — MEASURED, BECAUSE IT DECIDES WHAT THE FIXTURES MEAN.
// Go's encoding/json matches field names CASE-INSENSITIVELY, so `output_per_1M` (capital M) is
// NOT an unknown field: it lands on OutputPer1M and the reprice works. A rule that refused it
// would break documents that are correct today. TestDecodeOverrides_CaseInsensitiveSpellingIsNotATypo
// pins that, so this file's refusals are about fields Go genuinely cannot place.
//
// THE RULE: DecodeOverrides decodes the element with DisallowUnknownFields and refuses the WHOLE
// document, naming the element index. Whole-document refusal is #451's decision, restated rather
// than re-litigated: cmd/lens#applyCatalogOverrides applies NOTHING when the document does not
// parse, so "Lens could not read this" keeps ONE meaning. It costs nothing on the money axis —
// every model the document meant to price falls to ResolveRates' fallback arm, which is non-zero
// by construction and screams on every request.

// overrideDoc is one way to write the document, and what the rule must do with it.
type overrideDoc struct {
	name string
	doc  string
	// wantRefusedNaming is the substring the error must contain, or "" if the
	// document must be ACCEPTED.
	wantRefusedNaming string
}

// ─── rule O1: an unknown field is refused, and the refusal names it ───

func TestDecodeOverrides_RefusesAnUnknownField(t *testing.T) {
	cases := []overrideDoc{
		{
			// THE MOTIVATING CASE. Bills every output token at zero.
			name:              "input_per_1m right, output mistyped camelCase",
			doc:               `[{"id":"gpt-4o","input_per_1m":3.75,"outputPer1M":15.00}]`,
			wantRefusedNaming: "outputPer1M",
		},
		{
			// The same defect on the other rate.
			name:              "output_per_1m right, input mistyped camelCase",
			doc:               `[{"id":"gpt-4o","inputPer1M":3.75,"output_per_1m":15.00}]`,
			wantRefusedNaming: "inputPer1M",
		},
		{
			// tab-b7e1's stated case: on a NEW id, `base` is the zero Model, so a
			// misspelled provider registers Provider: "".
			name:              "provider misspelled on a new model",
			doc:               `[{"id":"brand-new-model","provder":"openai","input_per_1m":1.0,"output_per_1m":2.0}]`,
			wantRefusedNaming: "provder",
		},
		{
			// DisallowUnknownFields reaches nested objects too. A capability the
			// document thinks it is granting and is not is the #453 shape.
			name:              "unknown field inside capabilities",
			doc:               `[{"id":"gpt-4o","input_per_1m":1.0,"output_per_1m":2.0,"capabilities":{"vison":true}}]`,
			wantRefusedNaming: "vison",
		},
		{
			name:              "a field that is simply not part of Model",
			doc:               `[{"id":"gpt-4o","input_per_1m":1.0,"output_per_1m":2.0,"price":99}]`,
			wantRefusedNaming: "price",
		},
		{
			// The rule must survive the element not being the first one.
			name:              "the unknown field is in the SECOND element",
			doc:               `[{"id":"gpt-4o","input_per_1m":1.0,"output_per_1m":2.0},{"id":"gpt-4o-mini","input_per_1m":1.0,"outputPer1M":2.0}]`,
			wantRefusedNaming: "outputPer1M",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newNamedReg()
			_, err := r.DecodeOverrides([]byte(tc.doc))
			if err == nil {
				t.Fatalf("document was ACCEPTED and must be refused: %s", tc.doc)
			}
			if !strings.Contains(err.Error(), tc.wantRefusedNaming) {
				t.Fatalf("the refusal must NAME the field the operator got wrong (want %q): %v",
					tc.wantRefusedNaming, err)
			}
			// Naming the element is what makes a multi-element document actionable.
			if !strings.Contains(err.Error(), "element") {
				t.Fatalf("the refusal must name WHICH element: %v", err)
			}
		})
	}
}

// TestDecodeOverrides_RefusingIsWholeDocument: nothing from a refused document
// reaches the registry — including the elements that were spelled correctly.
// This is what makes "Lens could not read this" have one outcome.
func TestDecodeOverrides_RefusingIsWholeDocument(t *testing.T) {
	r := newNamedReg()
	inBefore, outBefore, ok := r.Price("gpt-4o")
	if !ok {
		t.Fatal("fixture registry does not know gpt-4o")
	}
	// The FIRST element is perfectly well formed; the second is not.
	doc := `[{"id":"gpt-4o","input_per_1m":9.99,"output_per_1m":99.99},{"id":"x","outputPer1M":1}]`
	if _, err := r.DecodeOverrides([]byte(doc)); err == nil {
		t.Fatal("document must be refused")
	}
	inAfter, outAfter, _ := r.Price("gpt-4o")
	if inAfter != inBefore || outAfter != outBefore {
		t.Fatalf("a refused document must change no price: gpt-4o went %.2f/%.2f -> %.2f/%.2f",
			inBefore, outBefore, inAfter, outAfter)
	}
}

// ─── rule O2: the FLOOR. A well-formed document is still accepted and applied ───
//
// Without this, rule O1 is satisfied by a decoder that refuses everything — and a
// catalog that cannot be repriced at all is a worse outcome than the defect.

func TestDecodeOverrides_WellFormedDocumentIsStillApplied(t *testing.T) {
	r := newNamedReg()
	// Every field of Model spelled correctly, including the optional ones, so
	// this floor fails if the rule refuses any legitimate key.
	doc := `[{"id":"gpt-4o","provider":"openai","display_name":"GPT-4o","input_per_1m":3.75,` +
		`"output_per_1m":15.00,"cached_input_per_1m":1.88,"cache_write_per_1m":4.69,` +
		`"capabilities":{"vision":true,"audio":false,"document":true},` +
		`"context_tokens":128000,"max_output":16384,"deprecated":false,"aliases":["gpt-4o-2024-05-13"]}]`
	ms, err := r.DecodeOverrides([]byte(doc))
	if err != nil {
		t.Fatalf("a well-formed document naming every field of Model must be accepted: %v", err)
	}
	if len(ms) != 1 {
		t.Fatalf("decoded %d elements, want 1", len(ms))
	}
	r.LoadOverrides(ms)
	in, out, ok := r.Price("gpt-4o")
	if !ok || in != 3.75 || out != 15.00 {
		t.Fatalf("the reprice must land: got %.2f/%.2f ok=%v, want 3.75/15.00", in, out, ok)
	}
	rates, prov := r.ResolveRates("gpt-4o", PurposeCharge)
	if prov != ProvenanceExact || rates.OutputPer1M != 15.00 {
		t.Fatalf("the repriced model must resolve exact at the stated price: %.2f/%.2f prov=%v",
			rates.InputPer1M, rates.OutputPer1M, prov)
	}
}

// TestDecodeOverrides_CaseInsensitiveSpellingIsNotATypo pins the measurement that
// decides what rule O1's fixtures mean. Go's encoding/json matches field names
// case-insensitively, so `output_per_1M` is NOT unknown — it lands on OutputPer1M
// and the reprice works. If the rule ever started refusing it, documents that are
// correct today would stop applying.
func TestDecodeOverrides_CaseInsensitiveSpellingIsNotATypo(t *testing.T) {
	r := newNamedReg()
	ms, err := r.DecodeOverrides([]byte(`[{"id":"gpt-4o","input_per_1m":3.75,"output_per_1M":15.00}]`))
	if err != nil {
		t.Fatalf("`output_per_1M` differs from `output_per_1m` only in case, and encoding/json "+
			"places it on OutputPer1M — refusing it would break documents that work today: %v", err)
	}
	if ms[0].OutputPer1M != 15.00 {
		t.Fatalf("case-insensitive match must still carry the price: got %.2f want 15.00", ms[0].OutputPer1M)
	}
}

// ─── rule O3: the money consequence, asserted where it is felt ───
//
// O1 asserts the document is refused. This asserts what refusing it BUYS: the
// motivating document no longer leaves gpt-4o billing output at zero on the arm
// that claims the catalog knows the price.

func TestDecodeOverrides_MistypedRateNeverBillsOutputAtZeroAsExact(t *testing.T) {
	r := newNamedReg()
	const doc = `[{"id":"gpt-4o","input_per_1m":3.75,"outputPer1M":15.00}]`

	if ms, err := r.DecodeOverrides([]byte(doc)); err == nil {
		// If the document is ever accepted again, say exactly what it costs.
		r.LoadOverrides(ms)
		rates, prov := r.ResolveRates("gpt-4o", PurposeCharge)
		t.Fatalf("the mistyped document was accepted: gpt-4o now resolves %.2f/%.2f provenance=%v — "+
			"every output token bills at %.2f on the arm that says the catalog knows this price",
			rates.InputPer1M, rates.OutputPer1M, prov, rates.OutputPer1M)
	}

	// The document was refused, so the seeded price stands and the money path is
	// unchanged — not merely "not zero", but the price that was there before.
	for _, purpose := range []struct {
		name string
		p    Purpose
	}{{"charge", PurposeCharge}, {"hold", PurposeHold}} {
		rates, prov := r.ResolveRates("gpt-4o", purpose.p)
		if prov != ProvenanceExact {
			t.Fatalf("%s: gpt-4o is a seeded model and must still resolve exact, got %v", purpose.name, prov)
		}
		if rates.OutputPer1M != 10.00 {
			t.Fatalf("%s: output rate is %.2f, want the seeded 10.00 — a refused document must leave "+
				"the price it failed to change exactly where it was", purpose.name, rates.OutputPer1M)
		}
	}
}

// ─── the PIN: the half this rule does NOT close, measured and named ───
//
// ⚠ THIS TEST PASSES ON A TREE WHERE THE DEFECT IS STILL PRESENT, BY DESIGN. It is
// a pin, not a guard: it records a measured gap so it cannot be quietly assumed
// closed, and it EXPIRES — the moment the gap is fixed, this test fails and names
// the decision that was taken.
//
// DisallowUnknownFields catches a field that is MISSPELLED. It cannot catch a field
// that is ABSENT, because absence is not an unknown key. DecodeOverrides zeroes the
// four rate fields before decoding (so a document STATES a price rather than merging
// into the seeded one — #450's "a reprice states a price"), so:
//
//	[{"id":"gpt-4o","input_per_1m":3.75}]   -> accepted, OutputPer1M 0.00, provenance EXACT
//
// An operator who means "only change the input price" gets zero-cost output tokens,
// silently, exactly like the mistyped case. THE FIX IS NOT OBVIOUS AND IT IS NOT A
// SESSION'S TO PICK: requiring both rate fields is a change to the override contract
// (and the three seeded embeddings models would then have to state "output_per_1m": 0
// explicitly); treating absent-as-unset and merging into the seeded price contradicts
// #450. Both are defensible and they bill differently. MEASURED AND REPORTED, NOT
// DECIDED — see the W6.1 handover.
func TestDecodeOverrides_PIN_AnOmittedRateIsStillAcceptedAsZero(t *testing.T) {
	r := newNamedReg()
	ms, err := r.DecodeOverrides([]byte(`[{"id":"gpt-4o","input_per_1m":3.75}]`))
	if err != nil {
		t.Fatalf("PIN EXPIRED — an omitted output_per_1m is now refused (%v). That is a decision about "+
			"the override contract; record it and delete this pin.", err)
	}
	r.LoadOverrides(ms)
	rates, prov := r.ResolveRates("gpt-4o", PurposeCharge)
	if prov != ProvenanceExact || rates.OutputPer1M != 0 {
		t.Fatalf("PIN EXPIRED — an omitted output_per_1m now resolves %.2f/%.2f provenance=%v instead of "+
			"billing output at zero on the exact arm. Record which way it was decided and delete this pin.",
			rates.InputPer1M, rates.OutputPer1M, prov)
	}
}
