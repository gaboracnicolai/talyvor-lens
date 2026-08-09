package discriminator

import "testing"

// ⚠ THE HOLE THIS FILE CLOSES, AND IT IS THE ONE THE PACKAGE DOC ALREADY FORBADE.
//
// discriminator.go's opening block says: "FAIL CLOSED. Every path that cannot positively verify a
// match must refuse." Extract returns NOTHING for a prompt naming no version, code, identifier,
// proper noun or listed technology — which is most consumer traffic — and Canon renders that as
// the empty string. Two empty strings compare EQUAL, so the gate answered "match" having verified
// nothing at all. The rule was written and never swept for.
//
// ⚠ MEASURED, not reasoned about (cmd/hitrate against the live embedder, 2026-08-09):
//
//	consumer  30/30 of the rephrase pairs the gate PASSED were equal-because-empty (100%)
//	consumer  28/29 of the danger  pairs the gate PASSED were equal-because-empty (97%)
//	engineering 3/17 of the rephrase pairs it passed (18%)
//
// So on consumer traffic the gate was not weak, it was INERT: it made no decision at all.
//
// ⚠ THE CONCRETE CASE. "How much notice does a landlord have to give…" and "…does a tenant have to
// give…" name the SAME entities and differ only in DIRECTION, which no extractor sees. They score
// 0.9770. Both canonicalise to "". Before this change the pool would serve one as the answer to
// the other, cross-tenant, on a paid path, with a royalty credited for it.
//
// ⚠ WHAT THIS DOES NOT CLOSE, said rather than implied: isa-year (0.9377) extracts "caps:isa" on
// at least one side, so it passes the gate for a REAL reason and is closed by the 0.98 threshold
// (6534aa5), not by this. Emptiness and wrongness are different failures.

func TestCanon_ConsumerPromptsExtractNothing(t *testing.T) {
	// Pinned so the premise of every test below is checked rather than assumed. If the extractor
	// ever learns to see these, this test fails and the reasoning above needs revisiting — which
	// is the point: the premise is a measurement, not a belief.
	for _, p := range []string{
		"How much notice does a landlord have to give before ending a tenancy?",
		"How much notice does a tenant have to give before ending a tenancy?",
	} {
		if got := Canon(p); got != "" {
			t.Fatalf("Canon(%q) = %q, want empty — this file's premise is that consumer prompts "+
				"name no extractable entity", p, got)
		}
	}
}

// TestMatch_EmptyExtractionIsNotAMatch is the rule itself.
func TestMatch_EmptyExtractionIsNotAMatch(t *testing.T) {
	a := "How much notice does a landlord have to give before ending a tenancy?"
	b := "How much notice does a tenant have to give before ending a tenancy?"

	if Match(a, b) {
		t.Fatalf("Match(%q, %q) = true — the gate verified NOTHING and called it a match. "+
			"Both sides extract no entity, and two empty sets comparing equal is the inert-gate "+
			"defect: 100%% of consumer rephrase passes and 97%% of consumer danger passes went "+
			"through this door.", a, b)
	}
}

// TestMatch_EmptyAgainstNonEmptyStillRefuses guards the direction that was already correct, so a
// fix cannot regress it into "empty matches everything".
func TestMatch_EmptyAgainstNonEmptyStillRefuses(t *testing.T) {
	if Match("How much notice must a landlord give?", "How do I write a validator in Pydantic v1?") {
		t.Fatal("an entity-less prompt matched an entity-bearing one")
	}
}

// ⚠ THE FLOOR. A gate that refuses EVERYTHING satisfies every test above and destroys the product.
// These pairs MUST still match, so "fail closed" cannot quietly become "fail always".
func TestMatch_EntityBearingPairsStillMatch(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		// same entity, freely varied wording — the traffic pooling exists to serve
		{"How do I write a validator in Pydantic v2?", "Pydantic v2: what is the way to validate a field?"},
		// alias folding must survive: "go" and "golang" are one entity
		{"How do I read lines in Go?", "How do I read lines in golang?"},
	} {
		if !Match(tc.a, tc.b) {
			t.Errorf("Match(%q, %q) = false — the gate now refuses a genuine rephrasing; "+
				"fail-closed has become fail-always", tc.a, tc.b)
		}
	}
}
