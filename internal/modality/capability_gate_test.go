package modality

import (
	"reflect"
	"testing"

	"github.com/talyvor/lens/internal/catalog"
)

// ─── the capability gate, per modality ───
//
// WHY THIS FILE EXISTS, MEASURED RATHER THAN ASSUMED. `Supports` is three
// independent branches — image, audio, document — and before this file the
// repo-wide serialised race suite (`go test -race -count=1 -p 1 ./...`, 98
// packages, the exact CI command) caught EXACTLY ONE of them:
//
//	image branch deleted    -> CAUGHT (3 proxy tests red)
//	audio branch deleted    -> NOT CAUGHT, exit 0, 98 ok, 0 FAIL
//	document branch deleted -> NOT CAUGHT, exit 0, 98 ok, 0 FAIL
//
// The product was CORRECT the whole time — a pinned audio request to gpt-4o
// answered 422 "does not support it" and recorded no spend. Nothing would
// have noticed if it stopped being correct. Measured on the wire with the
// audio + document branches removed, every one of these became a 200 with a
// spend row written: the caller is BILLED for a request whose audio/document
// content the model never received, which is verbatim the failure this
// package's own header says it exists to prevent ("silently flattening an
// image into text at a text-only model").
//
// WHY THE BLINDNESS WAS ONE-MODALITY-SHAPED — THE FIXTURES ARE UNIFORM:
// every capability assertion in the repo, in this package and in
// internal/proxy alike, was built from an IMAGE set. `Supports` was asserted
// only against `ModalitySet{HasImage: true}`; `CapableModel` likewise; every
// proxy body was an `image_url` block. A gate tested through one modality is
// tested in one branch, and the other two are prose.
//
// So the rules below are deliberately NOT "add two more cases". Two more
// cases would leave a fourth modality exactly as exposed as audio was:
//
//	rule A — a CENSUS, not exemplars: for EVERY model in the catalog and
//	         EVERY modality, Supports must agree with that model's recorded
//	         capability. A branch that stops biting reds here by name.
//	rule B — a FLOOR: each modality must have seen at least one capable AND
//	         one incapable model. A census over a population that is all-true
//	         (or all-false) cannot fail, and would go green over a deleted
//	         branch — so an empty side is a failure, loudly, naming the
//	         modality rather than silently shrinking the population.
//	rule C — CLOSURE: the table below must cover exactly the `Has*` bool
//	         fields of ModalitySet, read by reflection off the struct rather
//	         than restated. Add `HasVideo` without a gate and rule C reds; it
//	         is the half that makes A and B unable to narrow silently.

// gatedModality names one modality field of ModalitySet and how to build the
// request set + read the recorded capability for it. The `field` string is
// what rule C matches against the struct, so a rename cannot leave a stale
// entry pointing at nothing.
type gatedModality struct {
	name  string
	field string
	need  ModalitySet
	capOf func(catalog.Capabilities) bool
}

var gatedModalities = []gatedModality{
	{"image", "HasImage", ModalitySet{HasImage: true, ImageCount: 1}, func(c catalog.Capabilities) bool { return c.Vision }},
	{"audio", "HasAudio", ModalitySet{HasAudio: true, AudioCount: 1}, func(c catalog.Capabilities) bool { return c.Audio }},
	{"document", "HasDocument", ModalitySet{HasDocument: true}, func(c catalog.Capabilities) bool { return c.Document }},
}

// TestCapabilityGate_EveryModalityBitesOnEveryCatalogModel is rules A and B.
func TestCapabilityGate_EveryModalityBitesOnEveryCatalogModel(t *testing.T) {
	all := catalog.All()
	if len(all) == 0 {
		t.Fatal("catalog is empty — this census would pass over nothing")
	}

	for _, g := range gatedModalities {
		capable, incapable := 0, 0
		for _, m := range all {
			want := g.capOf(m.Capabilities)
			if got := Supports(m.ID, g.need); got != want {
				t.Errorf("rule A: Supports(%q, %s) = %v, want %v — the %s branch of the capability gate is not biting on this model",
					m.ID, g.name, got, want, g.name)
			}
			if want {
				capable++
			} else {
				incapable++
			}
		}
		// Rule B. Without BOTH sides the census above is satisfiable by a
		// gate that always answers the same thing, which is precisely what a
		// deleted branch does.
		if capable == 0 {
			t.Errorf("rule B: no catalog model is %s-capable, so the %s census can only ever assert refusals — it would stay green with the %s branch deleted",
				g.name, g.name, g.name)
		}
		if incapable == 0 {
			t.Errorf("rule B: every catalog model is %s-capable, so the %s census never exercises a refusal — it would stay green with the %s branch deleted",
				g.name, g.name, g.name)
		}
	}
}

// TestCapabilityGate_UnknownModelIsRefusedForEveryModality pins the
// package's stated conservative default ("a model whose capabilities are
// unknown is treated as text-only") for every gated modality, not just the
// image one it was written against. A model absent from the catalog is the
// ordinary case — an operator's override that has not landed, a provider's
// new id — and it is the input for which "we never assume a model can do
// vision" has to hold in all three branches.
func TestCapabilityGate_UnknownModelIsRefusedForEveryModality(t *testing.T) {
	const unknown = "totally-unknown-model-b7e1"
	if (catalog.CapabilitiesOf(unknown) != catalog.Capabilities{}) {
		t.Fatalf("%q is registered after all — this case needs a genuinely unknown id", unknown)
	}
	for _, g := range gatedModalities {
		if Supports(unknown, g.need) {
			t.Errorf("an unknown model must be treated as text-only and refuse %s content (conservative default)", g.name)
		}
	}
	// The companion that keeps the above from being satisfied by a gate that
	// refuses everything: a text-only request is served by any model.
	if !Supports(unknown, ModalitySet{}) {
		t.Error("a text-only request must be supported by every model, known or not")
	}
}

// TestCapabilityGate_TableCoversEveryModalityField is rule C: the table is
// checked against the struct, so a modality added to ModalitySet without a
// gate entry fails here rather than being silently ungated. Read by
// reflection — a restated list would rot exactly like the citations this
// repo has already had to repair.
func TestCapabilityGate_TableCoversEveryModalityField(t *testing.T) {
	declared := map[string]bool{}
	rt := reflect.TypeOf(ModalitySet{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if f.Type.Kind() == reflect.Bool && len(f.Name) > 3 && f.Name[:3] == "Has" {
			declared[f.Name] = true
		}
	}
	if len(declared) == 0 {
		t.Fatal("found no Has* bool fields on ModalitySet — the reflection rule is reading nothing")
	}

	covered := map[string]bool{}
	for _, g := range gatedModalities {
		if !declared[g.field] {
			t.Errorf("gatedModalities names %q, which is not a Has* bool field of ModalitySet — a renamed or removed modality has left a stale entry", g.field)
		}
		covered[g.field] = true
	}
	for f := range declared {
		if !covered[f] {
			t.Errorf("ModalitySet.%s is a modality with no entry in gatedModalities — it is not covered by the capability-gate census, so its branch of Supports (if any) is unguarded", f)
		}
	}
}
