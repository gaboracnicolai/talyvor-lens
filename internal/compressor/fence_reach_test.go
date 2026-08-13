package compressor

import (
	"context"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/poolsafety"
)

// EVERY CORPUS THIS PACKAGE MEASURES THE REWRITER AGAINST HAS ZERO FENCED CODE
// BLOCKS IN IT, SO ONE OF THE FOUR ADVERTISED TECHNIQUES IS MEASURED BY NOTHING.
//
// reach_test.go's headline is "308 prompts, 0 modified", and beside it "8 of 8
// agent-traffic prompts rewritten". Both are true. Neither population contains a
// ``` fence — measured below, 0 of 316 — so the code_blocks technique never runs
// on either. The reach measurement's boundary excludes the technique whose own
// machinery (the "\x00CODE<n>\x00" placeholder) turned out to relocate a
// customer's code block and transmit the gateway's marker to a provider
// (placeholder_collision_test.go).
//
// ⚠ THIS IS THE POPULATION PROBLEM, NOT A COVERAGE COMPLAINT. "0 of 308 modified"
// reads as a statement about the rewriter. It is a statement about the rewriter
// ON PROSE. The corpora are rephrase/danger pairs and agent diffs; nobody chose
// to exclude fenced code, and that is exactly why nothing noticed.
//
// The count is PINNED rather than asserted to be zero, so that adding fenced
// prompts to a corpus fails this test and forces the number in the comment above
// to be restated rather than silently outgrown.
func TestFenceReach_NoCorpusPromptReachesTheCodeBlockTechnique(t *testing.T) {
	prompts, fenced := allCorpusPrompts()

	if len(prompts) != 316 {
		t.Errorf("the measured corpora total %d prompts, pinned at 316 (308 filler0 + 8 agent) — "+
			"the population moved, so the zero below is no longer the zero that was measured", len(prompts))
	}
	if fenced != 0 {
		t.Errorf("%d corpus prompts now contain a fenced block, pinned at 0. That is GOOD NEWS and this "+
			"test must be rewritten to measure what the code_blocks technique does to them, not deleted", fenced)
	}
}

// THE POSITIVE CONTROL FOR THE COUNT ABOVE, THROUGH THE SAME LOOP.
//
// A census that reports zero is indistinguishable from a census that reads
// nothing, and this one reports zero on every input it has. Splice one fenced
// prompt into the same counting loop and it must report exactly one — otherwise
// the zero above is a property of the loop rather than of the corpora.
func TestFenceReach_TheFenceCountingLoopCanRegisterAHit(t *testing.T) {
	prompts, fenced := allCorpusPrompts()
	if fenced != 0 {
		t.Fatalf("premise: the corpora must start at 0 fenced, got %d", fenced)
	}

	spiked := append(append([]string(nil), prompts...), "explain:\n```go\nfunc a() {}\n```\n")
	if got := countFenced(spiked); got != 1 {
		t.Fatalf("splicing one fenced prompt into the census must report exactly 1, got %d", got)
	}
	if got := countFenced(prompts); got != 0 {
		t.Errorf("the real corpora must return to 0 fenced, got %d", got)
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

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
