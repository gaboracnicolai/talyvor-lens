package compressor

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/poolsafety"
)

// THE TECHNIQUE THAT WAS MEASURED BY NOTHING NOW HAS A POPULATION — AND THE FIRST
// THING THAT POPULATION SHOWED IS THAT THE FENCE IS NOT THE BOUNDARY.
//
// ⚠ WHAT THIS TEST USED TO SAY, AND WHY IT WAS REPLACED RATHER THAN DELETED. It
// pinned "0 of 316 corpus prompts contain a ``` fence" and instructed its own
// successor: "That is GOOD NEWS and this test must be rewritten to measure what
// the code_blocks technique does to them, not deleted." fence_corpus_test.go is
// that population — 17 ordinary prompts, a review request, a README, a truncated
// log paste, a numbered list of steps — and this is that measurement. Adding the
// corpus turned the old pins red (333 prompts, 15 fenced) before a line of it was
// rewritten.
//
// reach_test.go's headline is still "308 prompts, 0 modified" and beside it "8 of
// 8 agent-traffic prompts rewritten". Both were, and remain, statements about the
// rewriter ON PROSE. Nobody chose to exclude fenced code; that is exactly why
// nothing noticed for as long as it did.
//
// ⚠ TWO OF THE 17 CARRY NO ``` AT ALL, AND THAT GAP IS THE POINT RATHER THAN AN
// OVERSIGHT: a ~~~ fence and a four-space indented block are both code to
// CommonMark and neither is code to this rewriter (fence_pairing_test.go). Fenced
// coverage is counted at 15 so the corpus cannot claim them.
//
// ⚠ THIS TEST AND THE ONE BELOW PIN CONSTANTS, WHICH IS THE SHAPE THAT CANNOT BE
// RED-FIRST. Their controls are C30-C33 of w61-fencecorpus-controls-9d72.py:
// C30 shrinks the corpus to four entries, C31 and C32 blind the two counting
// instruments to the very constants pinned here (7 and 15), and C33 blinds the
// cross-check. C23-C26 of w61-fencereach-controls-4f2b.py cover the same tests
// from the compressCodeBlock side.
func TestFenceReach_TheFencedPopulationIsMeasured(t *testing.T) {
	prompts, fenced := allCorpusPrompts()

	if len(prompts) != 333 {
		t.Errorf("the measured corpora total %d prompts, pinned at 333 (308 filler0 + 8 agent + 17 fenced) — "+
			"the population moved, so the numbers below are no longer the numbers that were measured", len(prompts))
	}
	if fenced != 15 {
		t.Errorf("%d corpus prompts carry a ``` fence, pinned at 15 (17 fenced-corpus entries less the "+
			"~~~ fence and the four-space indented block, which carry no backticks by design)", fenced)
	}
	if got := fencedCorpusCarryingAFence(); got != 15 {
		t.Errorf("all 15 fenced prompts must come from the fenced corpus, but it contributes %d — "+
			"if a prose corpus grew a fence, the split above is wrong", got)
	}

	// AND THE MEASUREMENT THE OLD TEST ASKED FOR: how many of the population reach
	// the code_blocks technique at all. Seven — every one of them from the fenced
	// corpus, and five of those seven change the value of a multi-line literal.
	if got := reachingCodeBlocks(prompts); got != 7 {
		t.Errorf("%d of %d prompts reach the code_blocks technique, pinned at 7 — this is the number the "+
			"old census existed to make nonzero, so a move here is the headline, not a stale constant",
			got, len(prompts))
	}
}

// THE POSITIVE CONTROL FOR BOTH COUNTS, THROUGH THE SAME LOOPS.
//
// A census stuck at a constant is indistinguishable from a census that measures,
// once the constant stops being zero — which is exactly what changed when the
// fenced corpus arrived. So both directions are exercised: the prose-only
// population must still read 0 fenced and 0 code_blocks, and splicing one more
// fenced prompt in must move the count by exactly one.
func TestFenceReach_TheFenceCountingLoopMovesInBothDirections(t *testing.T) {
	prompts, fenced := allCorpusPrompts()
	if fenced != 15 {
		t.Fatalf("premise: the corpora must start at 15 fenced, got %d", fenced)
	}

	spiked := append(append([]string(nil), prompts...), "explain:\n```go\nfunc a() {}\n```\n")
	if got := countFenced(spiked); got != 16 {
		t.Fatalf("splicing one fenced prompt into the census must report exactly 16, got %d", got)
	}
	if got := countFenced(prompts); got != 15 {
		t.Errorf("the real corpora must return to 15 fenced, got %d", got)
	}

	// The prose-only half — the population the old "0 fenced" was measured on. It
	// must still read zero on both instruments, or the 15 above is the loop rather
	// than the corpus.
	//
	// ⚠ IT IS DERIVED FROM allCorpusPrompts BY SUBTRACTION, NOT REBUILT. The first
	// draft assembled it from filler0Corpora and poolsafety.Corpus directly, and
	// C26 of w61-fencereach-controls-4f2b.py — which deletes the agent-traffic
	// corpus from allCorpusPrompts — caught that: the rebuilt copy still counted
	// 316 while the census population had shrunk to 325. A second copy of a corpus
	// is not the corpus, which is the warning allCorpusPrompts' own doc carries.
	fencedTail := fencedCorpusPrompts()
	if len(prompts) <= len(fencedTail) {
		t.Fatalf("the census population (%d) does not exceed the fenced corpus (%d)", len(prompts), len(fencedTail))
	}
	prose := prompts[:len(prompts)-len(fencedTail)]
	if !reflect.DeepEqual(prompts[len(prose):], fencedTail) {
		t.Fatalf("the fenced corpus is no longer the TAIL of the census population, so subtracting it " +
			"does not yield the prose-only half — re-derive it rather than adjusting the numbers")
	}
	if len(prose) != 316 {
		t.Errorf("the prose-only population is %d prompts, pinned at 316", len(prose))
	}
	if got := countFenced(prose); got != 0 {
		t.Errorf("the prose-only population reports %d fenced, want 0", got)
	}
	if got := reachingCodeBlocks(prose); got != 0 {
		t.Errorf("the prose-only population reaches code_blocks %d times, want 0 — this is the zero the "+
			"old census reported, and it must survive the corpus being added beside it", got)
	}
}

// ⚠ AND HERE IS WHAT THE UNMEASURED TECHNIQUE ACTUALLY DOES: INSIDE THE FENCE —
// THE BOUNDARY THE NEIGHBOURING TEST CALLS PROTECTIVE — IT CHANGES THE VALUE OF A
// STRING LITERAL.
//
// TestReach_UnfencedPythonNestingCollapsesButFencedSurvives establishes that a
// fence protects INDENTATION, and its comment generalises that to "the ``` fence
// is the boundary". It is the boundary for indentation only. compressCodeBlock
// deletes every blank line strictly inside the fence and right-trims every line,
// and a Python triple-quoted string does not stop being a string because it is
// inside a code fence:
//
//	s = \"\"\"a
//
//	b\"\"\"        ->   s = \"\"\"a
//	                    b\"\"\"
//
// The caller asked about a program whose string is "a\n\nb". The provider is asked
// about a program whose string is "a\nb". That is a different program, and the
// technique reports it as a saving.
//
// ⚠ PINNED AS KNOWN BEHAVIOUR OF A GATED-OFF FEATURE, NOT FIXED, AND THE REASON IS
// A DECISION THIS SESSION WOULD NOT MAKE. Both of compressCodeBlock's operations
// are lossy for the same reason — blank-line removal and right-trimming each alter
// a multi-line literal's value — so making the technique lossless means either
// dropping it entirely (it has no other effect) or teaching it to parse the fenced
// language. That is a choice about what the product's one code-facing technique
// IS, and the design document's "keeps a subset of the original tokens in order"
// permits the deletion while saying nothing about literals. It belongs with the
// same reader who owns the holdout and the reduction technique. This test exists
// so a future re-enable has to argue with it and a future fix has to delete it
// deliberately — the pattern reach_test.go's four corruption cases already use.
func TestFenceReach_InsideAFenceABlankLineIsRemovedFromAStringLiteral(t *testing.T) {
	const in = "explain:\n```python\ns = \"\"\"a\n\nb\"\"\"\nprint(s)\n```\n"
	res := New().Compress(context.Background(), in)

	if !strings.Contains(in, "\"\"\"a\n\nb\"\"\"") {
		t.Fatal("premise: the input no longer carries the blank line inside the literal")
	}
	if strings.Contains(res.CompressedPrompt, "\"\"\"a\n\nb\"\"\"") {
		t.Errorf("the blank line inside the fenced string literal SURVIVED — the technique changed, so "+
			"this pinned corruption case must be deleted deliberately rather than left passing: %q",
			res.CompressedPrompt)
	}
	if !strings.Contains(res.CompressedPrompt, "\"\"\"a\nb\"\"\"") {
		t.Errorf("expected the literal to lose its blank line; got %q", res.CompressedPrompt)
	}
	// The rewriter credits itself for it.
	if !contains(res.TechniquesApplied, "code_blocks") {
		t.Errorf("expected 'code_blocks' among the techniques claimed for this change: %v", res.TechniquesApplied)
	}
}

// The second lossy operation, named separately because a fix for one is not a fix
// for the other. Right-trimming inside the fence removes trailing spaces that are
// part of the literal's value.
func TestFenceReach_InsideAFenceTrailingSpacesAreRemovedFromAStringLiteral(t *testing.T) {
	const in = "explain:\n```python\ns = \"\"\"a   \nb\"\"\"\nprint(s)\n```\n"
	res := New().Compress(context.Background(), in)

	if strings.Contains(res.CompressedPrompt, "a   \nb") {
		t.Errorf("the trailing spaces inside the fenced literal survived — the technique changed: %q",
			res.CompressedPrompt)
	}
	if !strings.Contains(res.CompressedPrompt, "\"\"\"a\nb\"\"\"") {
		t.Errorf("expected the literal's trailing spaces removed; got %q", res.CompressedPrompt)
	}
}

// allCorpusPrompts renders EVERY corpus reach_test.go measures the rewriter
// against, through the same helpers those measurements use, and counts how many
// carry a fence. Sharing the helpers is the point: a census assembled from its own
// copy of the corpora would not be a census of what is measured.
func allCorpusPrompts() (prompts []string, fenced int) {
	for _, s := range filler0Corpora() {
		prompts = append(prompts, s.prompts...)
	}
	for _, p := range poolsafety.Corpus() {
		prompts = append(prompts, p.Full(p.A), p.Full(p.B))
	}
	prompts = append(prompts, fencedCorpusPrompts()...)
	return prompts, countFenced(prompts)
}

func countFenced(prompts []string) int {
	n := 0
	for _, p := range prompts {
		if strings.Contains(p, "```") {
			n++
		}
	}
	return n
}

// reachingCodeBlocks counts prompts the rewriter credits the code_blocks technique
// on — i.e. prompts where compressCodeBlock actually changed a block's bytes.
// Counted from TechniquesApplied rather than from the presence of a fence,
// because a fence the pairing gets wrong produces no code_blocks hit at all: that
// difference is the finding, not an implementation detail.
func reachingCodeBlocks(prompts []string) int {
	c := New()
	ctx := context.Background()
	n := 0
	for _, p := range prompts {
		if contains(c.Compress(ctx, p).TechniquesApplied, "code_blocks") {
			n++
		}
	}
	return n
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
