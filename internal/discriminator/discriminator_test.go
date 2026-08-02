package discriminator

import (
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/poolsafety"
)

// These guard the gate's CLAIMS, not the similarity numbers that motivated it. The numbers belong
// to (embedding model × corpus) and will move; what must not move is that entity mismatches are
// refused, that the refusal generalises past the corpus it was designed on, and that ordinary
// rephrasing is not mistaken for an entity difference.

// ⚠ THE LIVE DEFECT, as data. These four score ABOVE the production threshold of 0.92 with
// text-embedding-3-small — 0.9579, 0.9450, 0.9265, 0.9207 — so no similarity threshold can refuse
// them. If this test fails, the pool is serving one version's answer for another's, cross-tenant.
func TestMatch_RefusesTheFourPairsThatPoolAboveThreshold(t *testing.T) {
	for _, c := range []struct{ name, a, b string }{
		{"pydantic-v1-v2", "How do I write a validator in Pydantic v1?", "How do I write a validator in Pydantic v2?"},
		{"vue-2-3", "How do I define a component in Vue 2?", "How do I define a component in Vue 3?"},
		{"tailwind-v3-v4", "How do I configure Tailwind CSS v3?", "How do I configure Tailwind CSS v4?"},
		{"router-v5-v6", "How do I define routes in React Router v5?", "How do I define routes in React Router v6?"},
	} {
		if Match(c.a, c.b) {
			t.Errorf("%s: gate ALLOWED a pair measured above the 0.92 threshold.\n  A=%q -> %q\n  B=%q -> %q\n"+
				"Nothing downstream can catch this: the vector distance says they are the same question.",
				c.name, c.a, Canon(c.a), c.b, Canon(c.b))
		}
	}
}

// The gate must refuse every pair in the corpus it was designed against.
func TestMatch_RefusesEveryDesignDangerPair(t *testing.T) {
	for _, p := range poolsafety.EngineeringDangerPairs() {
		if Match(p.A, p.B) {
			t.Errorf("ALLOWED %s\n  A=%q -> %q\n  B=%q -> %q", p.Name, p.A, Canon(p.A), p.B, Canon(p.B))
		}
	}
}

// ⚠ THE ONE THAT ACTUALLY MEANS SOMETHING. The design corpus produced these rules, so scoring 20/20
// against it measures fitting. HeldOutDangerPairs was written after the gate was frozen and is
// deliberately stocked with technologies absent from the lexicon.
//
// The bar is 18/20 rather than 20/20 because one residual is known and named: logrotate/journald
// are lowercase, unlisted, and carry no distinguishing SHAPE, so no token rule can separate them.
// Pinning the bar below perfect keeps this test honest about a limit that exists rather than
// quietly encoding the corpus into the lexicon to reach a round number.
func TestMatch_GeneralisesToHeldOutDangerPairs(t *testing.T) {
	pairs := poolsafety.HeldOutDangerPairs()
	var allowed []string
	for _, p := range pairs {
		if Match(p.A, p.B) {
			allowed = append(allowed, p.Name+" -> "+string(Canon(p.A))+" / "+string(Canon(p.B)))
		}
	}
	refused := len(pairs) - len(allowed)
	if refused < 18 {
		t.Errorf("generalisation dropped to %d/%d refused (want >=18). Allowed:\n  %s\n"+
			"A gate that only holds on the corpus that designed it has been fitted, not built.",
			refused, len(pairs), strings.Join(allowed, "\n  "))
	}
}

// ⚠ AND IT MUST NOT REFUSE EVERYTHING — the positive control for every test above. A gate that
// returns false unconditionally would pass all of them while deleting the product.
func TestMatch_AllowsGenuineRephrasings(t *testing.T) {
	var kept int
	for _, p := range poolsafety.EngineeringRephrasePairs() {
		if Match(p.A, p.B) {
			kept++
		}
	}
	if kept == 0 {
		t.Fatal("the gate refused ALL 28 genuine rephrasings — it has disabled pooling rather than " +
			"made it safe, and every refusal test above passes vacuously")
	}
	// Measured: 15 of 28 survive. Guarding the floor catches a rule that quietly turns into a
	// blanket refusal without having to re-pin the exact number on every corpus edit.
	if kept < 12 {
		t.Errorf("only %d/28 rephrasings allowed; the gate is over-refusing", kept)
	}
}

// Word order is a rephrasing, not an entity difference.
func TestCanon_IsOrderIndependent(t *testing.T) {
	a := Canon("In Pydantic v2, how do I write a validator?")
	b := Canon("How do I write a validator in Pydantic v2?")
	if a != b {
		t.Errorf("Canon differs by word order alone: %q vs %q", a, b)
	}
}

// ⚠ ALIASING IS WHAT KEEPS THE GATE FROM EATING ITS OWN TRAFFIC. "Go" and "golang" are one entity;
// a gate that called them different would refuse the highest-scoring rephrasing in the corpus
// (go-read-lines, 0.8487 — the single pair that a post-gate threshold can actually serve).
func TestCanon_AliasesSpellingsOfTheSameTechnology(t *testing.T) {
	for _, c := range [][2]string{
		{"How do I read a file in Go?", "How do I read a file in golang?"},
		{"How do I parse JSON in Python?", "How do I parse JSON in py?"},
		{"How do I connect to Postgres?", "How do I connect to PostgreSQL?"},
	} {
		if !Match(c[0], c[1]) {
			t.Errorf("alias not folded: %q -> %q vs %q -> %q", c[0], Canon(c[0]), c[1], Canon(c[1]))
		}
	}
}

// ⚠ A BARE VERB IS NOT AN ENTITY. Before command words were scoped to their owning tool, "merge two
// dictionaries" and "kill a process on port 3000" registered git/shell operations and refused
// genuine rephrasings while adding no safety.
func TestExtract_CommandWordsNeedTheirTool(t *testing.T) {
	if strings.Contains(string(Canon("How do I merge two dictionaries in Python?")), "cmd:merge") {
		t.Error("'merge' registered as a git operation in a Python question about dictionaries")
	}
	if !strings.Contains(string(Canon("How do I merge a branch in git?")), "cmd:merge") {
		t.Error("'merge' did NOT register when git is named — the ownership rule has disabled the class")
	}
}

// The structural tier must see entities nobody listed — that is the whole point of keying on shape.
func TestExtract_StructuralTierSeesUnlistedEntities(t *testing.T) {
	for _, c := range []struct{ prompt, want string }{
		{"Why does my Rust build fail with E0382?", "code:e0382"},
		{"What does SIGTERM do?", "caps:sigterm"},
		{"How do I define a model in Prisma?", "propn:prisma"},
		{"How do I read environment variables in Deno?", "propn:deno"},
	} {
		if !strings.Contains(string(Canon(c.prompt)), c.want) {
			t.Errorf("%q -> %q, missing %q. Unlisted technologies must still register, or the "+
				"lexicon's edge becomes a silent hole that pools two different answers.",
				c.prompt, Canon(c.prompt), c.want)
		}
	}
}

// A sentence-initial capital names nothing; treating it as a proper noun would refuse on the first
// word of every question.
func TestExtract_SentenceInitialWordsAreNotProperNouns(t *testing.T) {
	if strings.Contains(string(Canon("Command to list files")), "propn:command") {
		t.Error("sentence-initial 'Command' registered as a proper noun")
	}
}
