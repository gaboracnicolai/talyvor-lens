package canonq

import (
	"strings"
	"testing"
)

// ⚠ THE DEFECT CLASS THIS FILE EXISTS FOR: a canonicaliser is an LLM, and an LLM has a
// CONSTANT-OUTPUT failure mode that a similarity threshold does not. If the model refuses,
// apologises, or emits a generic placeholder, EVERY prompt that triggers it produces the SAME
// string — and under "hash the canonical form, match exactly" identical strings are a POOL HIT.
// A refusal is therefore not a missing measurement, it is a mass false serve, and the danger
// corpus (paracetamol dosing, insulin, pregnancy) is exactly the traffic that provokes one.
// Parse must reject a non-question rather than hand it to Key.

func TestParseRejectsRefusalsAndPreamble(t *testing.T) {
	// Every one of these is a real reply shape a small model produces, and every one would
	// collapse two unrelated prompts to one key if it reached Key.
	refusals := []string{
		"I'm sorry, I can't help with that.",
		"I cannot provide medical advice.",
		"Sorry, I can't assist with that request.",
		"I'm unable to help with this.",
		"As an AI language model, I cannot answer this.",
		"",
		"   \n  \t ",
		// ⚠ THE FOUR BELOW ARE THE ONLY ONES THIS TEST ACTUALLY EARNS. Measured by positive
		// control: deleting the refusal-prefix check entirely left this test GREEN on the seven
		// above, because every one of them is ALSO rejected by the question-shape test — they
		// open with "I'm"/"As", not an interrogative, and carry no "?". Seven fixtures agreeing
		// is one fixture and six copies. A refusal that is QUESTION-SHAPED is the only input that
		// can tell the two checks apart, and a small model asking to clarify emits exactly this.
		"I'm sorry, can you rephrase that?",
		"Sorry, what exactly do you want to know?",
		"Unfortunately, is there anything else I can help with?",
		"I cannot answer that — can you give me more detail?",
	}
	for _, r := range refusals {
		if got := Parse(r); got != "" {
			t.Errorf("Parse(%q) = %q, want \"\" — a refusal that becomes a key is a shared key", r, got)
		}
	}
}

func TestParseAcceptsACanonicalQuestion(t *testing.T) {
	cases := map[string]string{
		"what is the capital of the United Kingdom":   "what is the capital of the United Kingdom",
		"  what is the capital of France?\n":          "what is the capital of France?",
		"\"how long do you boil an egg\"":             "how long do you boil an egg",
		"Canonical question: what is a goroutine":     "what is a goroutine",
		"what does ImportError mean in Python\nNote:": "what does ImportError mean in Python",
	}
	for in, want := range cases {
		if got := Parse(in); got != want {
			t.Errorf("Parse(%q) = %q, want %q", in, got, want)
		}
	}
}

// ⚠ A LONG REPLY IS AN ANSWER, NOT A CANONICAL FORM. A model that starts explaining has left the
// task; hashing its explanation keys the pool on prose nobody will ever type again.
func TestParseRejectsAnAnswerLengthReply(t *testing.T) {
	long := "the capital of the United Kingdom is London, which has been the seat of government since " +
		"the twelfth century and is the largest city in the country by a considerable margin, with"
	if got := Parse(long); got != "" {
		t.Errorf("Parse(long prose) = %q, want \"\"", got)
	}
}

// Fold is the ONLY deterministic step. It runs on the MODEL'S OUTPUT and never on the user's
// prompt — W2.5 measured that folding the prompt itself deletes the capital-anchored
// discriminator tokens the entity gate is built from (74 tokens across 22.7% of the corpus).
func TestFoldIsTypographicOnly(t *testing.T) {
	cases := map[string]string{
		"What is the capital of the UK?":   "what is the capital of the uk",
		"what  is\tthe capital of the UK":  "what is the capital of the uk",
		"  what is the capital of the UK ": "what is the capital of the uk",
		"What is the capital of the UK!!":  "what is the capital of the uk",
		"what is the capital of the UK.":   "what is the capital of the uk",
	}
	for in, want := range cases {
		if got := Fold(in); got != want {
			t.Errorf("Fold(%q) = %q, want %q", in, got, want)
		}
	}
}

// ⚠ FOLD MUST NOT COLLAPSE MEANING. These four pairs differ by ONE entity and have different
// correct answers; if the typographic fold makes any of them equal, the fold is doing semantic
// work it was not authorised to do.
func TestFoldDoesNotCollapseDifferentQuestions(t *testing.T) {
	pairs := [][2]string{
		{"how much paracetamol can I take", "how much paracetamol can a child take"},
		{"when must a landlord give notice", "when must a tenant give notice"},
		{"what does ImportError mean", "what does ModuleNotFoundError mean"},
		{"how do I profile memory", "how do I profile CPU"},
	}
	for _, p := range pairs {
		if Fold(p[0]) == Fold(p[1]) {
			t.Errorf("Fold collapsed %q and %q", p[0], p[1])
		}
	}
}

// ⚠ THE µ PIN W2.5 LEFT BEHIND. U+00B5 MICRO SIGN and U+03BC GREEK SMALL MU are both already
// lower case, so ToLower leaves them distinct and two spellings of the same word key apart.
// Fold unifies them, and this test states the direction so a future change cannot flip it
// silently.
func TestFoldUnifiesTheTwoMuSigns(t *testing.T) {
	micro := "what is µLENS" // U+00B5 MICRO SIGN
	greek := "what is μLENS" // U+03BC GREEK SMALL LETTER MU
	if Fold(micro) != Fold(greek) {
		t.Errorf("Fold(µ)=%q Fold(μ)=%q — the two mu spellings key apart", Fold(micro), Fold(greek))
	}
}

func TestKeyIsStableAndDistinct(t *testing.T) {
	a := Key("What is the capital of the UK?")
	b := Key("what is the capital of the uk")
	c := Key("what is the capital of France")
	if a != b {
		t.Errorf("Key is not fold-stable: %s vs %s", a, b)
	}
	if a == c {
		t.Error("Key collided on two different questions")
	}
	if len(a) != 64 {
		t.Errorf("Key length %d, want 64 hex chars", len(a))
	}
}

// ⚠ AN EMPTY CANONICAL FORM MUST NOT PRODUCE A KEY AT ALL. Key("") is a perfectly good hash of
// the empty string, and every prompt the canonicaliser failed on would share it. The zero value
// has to be unusable, not merely unlikely.
func TestKeyRefusesTheEmptyForm(t *testing.T) {
	if Key("") != "" {
		t.Errorf("Key(\"\") = %q, want \"\" — a shared key for every failure is a mass false serve", Key(""))
	}
	if Key("   ") != "" {
		t.Errorf("Key(whitespace) = %q, want \"\"", Key("   "))
	}
}

// The prompt is part of the measured artefact: a canonicaliser measured under one instruction
// says nothing about another. Pin the properties the measurement depends on.
func TestPromptStatesTheInvariantsTheMeasurementRestsOn(t *testing.T) {
	for _, needle := range []string{"%s", "ONLY"} {
		if !strings.Contains(Prompt, needle) {
			t.Errorf("Prompt is missing %q", needle)
		}
	}
}
