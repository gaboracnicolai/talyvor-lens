package proxy

// W2.5 TIER-1 CANONICALISATION — MEASURED AND REFUSED, AND PINNED SO IT STAYS REFUSED.
//
// The proposal: normalise the prompt before hashing — lowercase, collapse whitespace, strip
// terminal punctuation, nothing else — on the argument that it "is still exact matching and
// carries none of the semantic risk". Both halves of that argument were measured against the
// committed corpora and both are wrong in the same direction.
//
// ⚠ WHAT IT BUYS: NOTHING. Not "a point or two" — zero, on both corpora, at every threshold.
// Of 30 ENGINEERING and 38 CONSUMER rephrase pairs, ZERO collapse to the same canonical form,
// and of 44 + 42 danger pairs, ZERO do either. cmd/hitrate re-run with both sides canonicalised
// reports the production hit column IDENTICAL to raw at 0.98/0.95/0.92/0.88 (ENGINEERING 2/30
// at every threshold, CONSUMER 0/38 at every threshold), and the entity-gate ceiling — the
// threshold-independent number cmd/hitrate's own header says bounds the product — unmoved at
// 14/30 and 0/38. The corpora are adversarial by construction and hold no typographic variants,
// which is exactly the "real result" W2.5 said to report and stop on.
//
// ⚠ WHAT IT COSTS: THE ENTITY GATE, WHICH IS THE ONLY THRESHOLD-INDEPENDENT GUARD. There is ONE
// prompt string in serve(): extractPrompt derives it, and it is the SAME variable that reaches
// exact.Key (the private cache), pooledPromptKey (the CROSS-TENANT exact cache), and
// discriminator.Canon (the pooled semantic gate's stored value and its `discriminators = $6`
// comparand). Canonicalise it "before hashing" and you have canonicalised it before the gate too.
// discriminator's extractors are anchored on capitals — reCaps is `[A-Z]{2,}`, rePropn is
// `[A-Z][a-z]{2,}`, reIdent's first alternative is CamelCase — so lowercasing deletes those
// classes outright: 74 tokens (caps:44, propn:30, id:5) vanish across 22.7% of corpus prompts,
// and ENGINEERING danger pairs passing the gate go 0/44 -> 3/44. Named, because they are the
// point: ImportError/ModuleNotFoundError, Kubernetes Deployment/StatefulSet, and profile
// memory/CPU stop being distinguishable to the gate and fall back to resting on the cosine
// threshold alone.
//
// ⚠ AND IT IS SILENT. MEASURED, NOT READ: canonicalising inside extractPrompt and running the
// whole suite leaves the failure set BYTE-IDENTICAL to a clean tree. Nothing in this repository
// notices that every prompt in the product is being lowercased before the cache key, the
// cross-tenant key and the gate. That is what this file exists to change.
//
// ⚠ IT ALSO DOES NOT SOLVE THE µ PROBLEM IT WAS WARNED ABOUT. strings.ToLower leaves U+00B5
// MICRO SIGN and U+03BC GREEK SMALL LETTER MU distinct — both are already lower case — so
// canon("µLENS") != canon("μLENS"). strings.EqualFold says they ARE equal, and ToUpper(U+00B5)
// is U+039C, so a ToUpper/ToLower round trip yields a third answer. Three plausible spellings of
// "lowercase", three different key sets, and zero micro-sign strings in any committed corpus, so
// no existing harness can tell them apart. Pinned in TestMicroSignIsNotUnifiedByLowercasing.
//
// This file changes no serve path and endorses no threshold. It pins the two facts that make the
// proposal unsafe, so the next session has to re-measure rather than re-argue.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/discriminator"
)

func chatBody(t *testing.T, msgs ...string) []byte {
	t.Helper()
	type m struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	req := struct {
		Model    string `json:"model"`
		Messages []m    `json:"messages"`
	}{Model: "gpt-4o"}
	for _, s := range msgs {
		req.Messages = append(req.Messages, m{Role: "user", Content: s})
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// extractPrompt is the ONE seam. Whatever it returns is hashed into the private key, into the
// ungated cross-tenant pooled key, and extracted into the pooled gate's discriminators. It must
// hand back the prompt the caller sent, byte for byte.
func TestExtractPromptIsVerbatim_NoCanonicalisation(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		// Case is load-bearing: CPU is a caps discriminator, ImportError an id one.
		{"caps-entity", "How do I profile CPU usage in Python?"},
		{"camelcase-entity", "Why does my Python script raise ImportError?"},
		// Whitespace and terminal punctuation are the other two thirds of the proposal.
		{"internal-whitespace", "How  do   I tie a tie?"},
		{"terminal-punctuation", "What is the capital of the UK?"},
		{"trailing-space", "What is photosynthesis? "},
	}
	for _, c := range cases {
		_, got, err := extractPrompt(chatBody(t, c.in))
		if err != nil {
			t.Fatalf("%s: extractPrompt: %v", c.name, err)
		}
		if got != c.in {
			t.Errorf("%s: extractPrompt normalised the prompt.\n got  %q\n want %q\n"+
				"This string is hashed into exact.Key AND pooledPromptKey (cross-tenant, NO entity "+
				"gate — proxy.go justifies that exemption by byte-identity) AND passed to "+
				"discriminator.Canon. W2.5 measured canonicalisation here as +0 hit rate on both "+
				"corpora and 0/44 -> 3/44 danger pairs through the gate. Re-measure before changing it.",
				c.name, got, c.in)
		}
	}
}

// The mechanism, stated as a property of the extractor rather than of any one prompt: the
// capital-anchored classes do not survive lowercasing. If this ever goes green-with-a-different-
// answer, the case-anchoring changed and W2.5's arithmetic has to be redone.
func TestDiscriminatorExtractorIsCaseAnchored(t *testing.T) {
	cases := []struct {
		name, prompt, class, value string
	}{
		{"caps", "How do I profile CPU usage in Python?", "caps", "cpu"},
		{"propn", "How do I write a Kubernetes Deployment?", "propn", "deployment"},
		{"id", "Why does my Python script raise ImportError?", "id", "importerror"},
	}
	for _, c := range cases {
		if !hasToken(discriminator.Extract(c.prompt), c.class, c.value) {
			t.Fatalf("%s: premise failed — %s:%s is not extracted from %q even before lowercasing; "+
				"this test is measuring nothing", c.name, c.class, c.value, c.prompt)
		}
		if hasToken(discriminator.Extract(strings.ToLower(c.prompt)), c.class, c.value) {
			t.Errorf("%s: %s:%s SURVIVED lowercasing. The extractor is no longer case-anchored, so "+
				"W2.5's measured cost (74 tokens lost, 22.7%% of corpus prompts, danger pairs through "+
				"the gate 0/44 -> 3/44) no longer describes this code. Re-run cmd/hitrate canonical "+
				"vs raw before relying on either number.", c.name, c.class, c.value)
		}
	}
}

// The consequence, on the three pairs that carry it. Each MUST NOT be served the other's answer.
// Raw, the gate refuses them without consulting any threshold. Lowercased, it admits them.
func TestLowercasingOpensTheEntityGateOnDangerPairs(t *testing.T) {
	pairs := []struct{ name, a, b string }{
		{"py-import-errors",
			"Why does my Python script raise ImportError?",
			"Why does my Python script raise ModuleNotFoundError?"},
		{"k8s-workload-kind",
			"How do I write a Kubernetes Deployment?",
			"How do I write a Kubernetes StatefulSet?"},
		{"profile-resource",
			"How do I profile memory usage in Python?",
			"How do I profile CPU usage in Python?"},
	}
	for _, p := range pairs {
		if discriminator.Match(p.a, p.b) {
			t.Errorf("%s: the gate ALREADY admits this danger pair on raw prompts — the pooled "+
				"selector's `discriminators = $6` is not separating them and only the threshold is.",
				p.name)
			continue
		}
		if !discriminator.Match(strings.ToLower(p.a), strings.ToLower(p.b)) {
			t.Errorf("%s: lowercasing no longer opens the gate on this pair. That is a CHANGE in "+
				"the extractor, not a fix here — W2.5's 0/44 -> 3/44 must be re-measured.", p.name)
		}
	}
}

// W2.5 asked for the micro sign to be pinned. It is not one question but three, and the three
// disagree — which is the finding, since no committed corpus contains a micro sign at all.
func TestMicroSignIsNotUnifiedByLowercasing(t *testing.T) {
	const microSign = "µ" // MICRO SIGN — what a keyboard and the µLENS/µLXC unit names produce
	const greekMu = "μ"   // GREEK SMALL LETTER MU
	const capitalMu = "Μ" // GREEK CAPITAL LETTER MU

	if strings.ToLower(microSign) != microSign {
		t.Errorf("ToLower(U+00B5) = %U, want U+00B5 unchanged", []rune(strings.ToLower(microSign)))
	}
	if strings.ToLower(greekMu) != greekMu {
		t.Errorf("ToLower(U+03BC) = %U, want U+03BC unchanged", []rune(strings.ToLower(greekMu)))
	}
	// The trap, stated three ways.
	if strings.ToLower(microSign) == strings.ToLower(greekMu) {
		t.Error("ToLower unified µ and μ — it does not in this Go version, and W2.5's note that " +
			"lowercasing does NOT fix the micro-sign split would no longer hold")
	}
	if !strings.EqualFold(microSign, greekMu) {
		t.Error("EqualFold no longer folds µ to μ — the disagreement between ToLower and EqualFold " +
			"is the whole point of this pin")
	}
	if strings.ToUpper(microSign) != capitalMu {
		t.Errorf("ToUpper(U+00B5) = %U, want U+039C — a ToUpper/ToLower round trip is the third, "+
			"different answer", []rune(strings.ToUpper(microSign)))
	}
}

func hasToken(ts []discriminator.Token, class, value string) bool {
	for _, t := range ts {
		if t.Class == class && t.Value == value {
			return true
		}
	}
	return false
}
