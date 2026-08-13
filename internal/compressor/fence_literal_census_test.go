package compressor

import (
	"context"
	"strings"
	"testing"
)

// THE NUMBER THAT QUANTIFIES THE HARM WAS THE ONE NUMBER NOTHING COMPUTED.
//
// fence_reach_test.go pins, and positive-controls, that 7 of 333 corpus prompts
// reach the code_blocks technique. Beside that count its doc carried a second
// figure — "five of those seven change the value of a multi-line literal" — and
// no instrument in this package produced it. It is the sentence a reader weighs
// when deciding whether to re-enable the feature: "7 prompts touched" is
// harmless if the touches are the JSON and SQL blank lines the corpus calls a
// REAL saving, and is a content corruption on a money path if they are string
// literals. The two readings differ only by that figure.
//
// MEASURED, IT IS THREE, NOT FIVE. The seven code_blocks hits are: the JSON
// blank line and the SQL blank line (lossless, and the corpus says so), the two
// Python triple-quoted literals and the Go backtick raw string (lossy — three),
// the Go fixture whose literal is escaped onto ONE source line and therefore
// survives the right-trim of the line below it, and the nested README, where
// what changes is a markdown document's blank lines and the inner block's
// indentation — neither is a literal. Five was not measured from the corpus; it
// could not have been, because until this file nothing in the package knew
// which bytes of a prompt were inside a literal.
//
// So this file makes the claim falsifiable rather than deleting it. Each corpus
// entry declares its literals verbatim (fencedPrompt.literals), every
// declaration is checked to appear in the prompt BEFORE any survival conclusion
// is drawn from it, and the count of entries losing one is pinned. A future
// change to compressCodeBlock now moves a number instead of a paragraph.
//
// ⚠ THE PREMISE CHECK IS THE LOAD-BEARING PART. A literal that is not in the
// prompt survives vacuously: mistype `"""a\n\nb"""` and the entry silently joins
// the lossless column, moving the headline in the flattering direction. That is
// the same shape as a census whose counting instrument reads nothing — see
// C25 in this item's history, where `countFenced` returning 0 still reported a
// passing "0 fenced".
//
// Controls: w61-literalcensus-controls-3e7b.py, both directions.

// literalsDestroyed returns the declared literals of p that do NOT survive
// byte-identically in out. Substring presence, deliberately: it asks the only
// question the caller cares about — are the exact bytes the caller wrote still
// the bytes the provider is asked about — without this package acquiring an
// opinion about any language's grammar.
func literalsDestroyed(p fencedPrompt, out string) []string {
	var gone []string
	for _, lit := range p.literals {
		if !strings.Contains(out, lit) {
			gone = append(gone, lit)
		}
	}
	return gone
}

func TestFenceCorpus_LiteralCensus(t *testing.T) {
	c := New()
	ctx := context.Background()

	declaring, destroyed, surviving := 0, 0, 0
	totalLiterals := 0

	for _, p := range fencedCorpus() {
		// THE HOLE IN A CENSUS DRAWN FROM DECLARATIONS: an entry that carries a
		// literal and declares none is counted as lossless without being measured,
		// and it is counted that way SILENTLY. Sweep for the one delimiter that can
		// be recognised without parsing anything.
		//
		// ⚠ HONEST LIMIT, SAID RATHER THAN IMPLIED: this sweep sees Python
		// triple-quotes ONLY. A Go backtick raw string cannot be told from the ```
		// fences around it by counting, so the go entry below rests on its
		// declaration alone — which is why `declaring` is pinned as well as
		// `destroyed`. This catches the forgetful case for one language, not all.
		if strings.Contains(p.in, `"""`) && len(p.literals) == 0 {
			t.Errorf("%s: the prompt carries a \"\"\" literal and declares none — it would "+
				"count as lossless without ever being measured", p.name)
		}
		if len(p.literals) == 0 {
			continue
		}
		declaring++
		totalLiterals += len(p.literals)

		// PREMISE, BEFORE ANY CONCLUSION: the declared literal is really in the
		// prompt. Without this, "it survived" is indistinguishable from "it was
		// never there".
		for _, lit := range p.literals {
			if !strings.Contains(p.in, lit) {
				t.Fatalf("%s: declared literal %q does not appear in the prompt — "+
					"a literal that is not there cannot be destroyed, so this entry would "+
					"count as lossless without measuring anything", p.name, lit)
			}
		}

		out := c.Compress(ctx, p.in).CompressedPrompt
		if gone := literalsDestroyed(p, out); len(gone) > 0 {
			destroyed++
			// A destroyed literal must belong to a prompt the rewriter credits
			// code_blocks on. If it ever does not, the loss came from a technique
			// that does not claim to touch code, which is a bigger finding than
			// this count.
			if tech := c.Compress(ctx, p.in).TechniquesApplied; !contains(tech, "code_blocks") {
				t.Errorf("%s: a literal was destroyed but code_blocks is not among the "+
					"credited techniques %v — the loss is attributed to the wrong technique", p.name, tech)
			}
		} else {
			surviving++
		}
	}

	// BOTH DIRECTIONS MUST BE POPULATED, or the checker is a constant wearing a
	// loop. If nothing survives, `literalsDestroyed` may be answering "yes"
	// unconditionally; if nothing is destroyed, "no".
	if destroyed == 0 {
		t.Error("no declared literal is destroyed anywhere in the corpus — the survival " +
			"check cannot be shown to detect a loss at all")
	}
	if surviving == 0 {
		t.Error("no declared literal survives anywhere in the corpus — the survival " +
			"check cannot be shown to detect anything BUT a loss")
	}

	if declaring != 4 {
		t.Errorf("%d corpus entries declare a literal, pinned at 4 — declarations are the "+
			"population this count is drawn from, so losing one lowers the harm figure "+
			"without changing the rewriter", declaring)
	}
	if totalLiterals != 4 {
		t.Errorf("%d literals declared in total, pinned at 4", totalLiterals)
	}
	if destroyed != 3 {
		t.Errorf("%d of the 7 code_blocks hits change the value of a declared literal, pinned at 3 — "+
			"the doc claimed 5 and nothing computed it; if this moves, the harm figure in "+
			"fence_reach_test.go moves with it", destroyed)
	}
}
