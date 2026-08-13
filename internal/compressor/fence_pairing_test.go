package compressor

import (
	"context"
	"strings"
	"testing"
)

// ⚠⚠ THE ``` FENCE IS NOT THE BOUNDARY. THE PAIRING IS — AND ONE STRAY ``` ANYWHERE
// EARLIER IN THE PROMPT INVERTS IT FOR EVERYTHING THAT FOLLOWS.
//
// codeBlockRE is `(?s)```.*?``` `. It walks the prompt left to right and pairs
// backtick runs in the order it meets them, with no notion of which run OPENS a
// block. So the regions it protects are the odd-numbered gaps between backtick
// runs, and the regions it hands to the prose techniques are the even-numbered
// ones. When every ``` in the prompt is either an opener or its own closer those
// two sets coincide with "code" and "prose". When ONE extra run appears — a
// sentence that mentions ```, a README that contains a fenced example, a paste
// truncated mid-block — the two sets swap for the whole remainder of the prompt.
//
// The result is not a missed saving. It is the corruption the fence was supposed
// to prevent, happening INSIDE a fence the caller wrote, while the rewriter reports
// only "whitespace" and never credits code_blocks at all. Python nesting collapses;
// a diff's tabs collapse; a log's columns collapse. Measured on ordinary prompts —
// fence_corpus_test.go — not on adversarial ones.
//
// ⚠ PINNED AS KNOWN BEHAVIOUR OF A GATED-OFF FEATURE, NOT FIXED, AND THE REASON IS
// A DECISION THIS SESSION WOULD NOT MAKE ALONE. A correct fix is a CommonMark fence
// scanner: a fence opens only at the start of a line under at most three spaces of
// indent, closes on a run at least as long with nothing after it, and ~~~ opens one
// too. That is a markdown parser in a package whose stated phase-1 scope is "JSON
// dedup, tree-sitter AST trimming, log collapse" — a choice about what the
// product's one code-facing technique IS, and it belongs with the same reader who
// owns the holdout and the reduction technique. These tests exist so a re-enable
// has to argue with them and a fix has to delete them deliberately.
//
// ⚠ EVERY TEST IN THIS FILE PASSED ON ITS FIRST RUN — each states something the
// shipped rewriter already does — so the controls are the evidence, not the
// tests. C27 of w61-fencecorpus-controls-9d72.py applies the CommonMark fix as a
// mutation (line-anchor the fence) and the two inversion tests must go red while
// the nested-README one holds, because line anchoring does not reach it.

// THE DIFFERENTIAL, AND IT IS THE WHOLE FINDING IN ONE ASSERTION. The same fenced
// python block, in the same prompt, protected or destroyed depending only on
// whether an earlier SENTENCE happens to mention ```.
func TestFencePairing_OneStrayFenceInProseFlipsTheSameBlockFromProtectedToDestroyed(t *testing.T) {
	c := New()
	ctx := context.Background()
	const block = "```python\ndef f():\n    if x:\n        return 1\n```\n"

	protected := c.Compress(ctx, "Wrap it in fences, like:\n"+block).CompressedPrompt
	if !strings.Contains(protected, "\n    if x:\n        return 1") {
		t.Fatalf("premise: with an even number of runs the block must survive verbatim; got %q", protected)
	}

	// The ONLY difference is the word "fences" becoming "``` fences" in the prose.
	destroyed := c.Compress(ctx, "Wrap it in ``` fences, like:\n"+block).CompressedPrompt
	if strings.Contains(destroyed, "\n    if x:\n        return 1") {
		t.Errorf("the pairing was fixed — the same block now survives a stray ``` in the prose. "+
			"That is the fix this file exists to force a deliberate deletion for: %q", destroyed)
	}
	if !strings.Contains(destroyed, "\n if x:\n return 1") {
		t.Errorf("expected both python nesting levels flattened to one space inside the caller's fence; got %q", destroyed)
	}

	// AND THE ATTRIBUTION IS SILENT ABOUT IT. The rewriter reports whitespace only:
	// nothing in TechniquesApplied says a fenced region was treated as prose, so an
	// audit that watches the technique list cannot see this class at all.
	res := c.Compress(ctx, "Wrap it in ``` fences, like:\n"+block)
	if contains(res.TechniquesApplied, "code_blocks") {
		t.Errorf("code_blocks was credited on the inverted prompt; the attribution changed: %v", res.TechniquesApplied)
	}
	if !contains(res.TechniquesApplied, "whitespace") {
		t.Errorf("expected the prose technique to be the only one credited; got %v", res.TechniquesApplied)
	}
}

// THE INVERSION IS NOT LOCAL. One stray run shifts every pairing after it, so a
// prompt with two real blocks loses both — and the prose between them becomes the
// "protected" region.
func TestFencePairing_OneStrayFenceInvertsEveryBlockAfterIt(t *testing.T) {
	const in = "Use ``` to fence.\n```go\nfunc a() {\n\tx := 1\n}\n```\nand\n```go\nfunc b() {\n\ty := 2\n}\n```\n"
	out := New().Compress(context.Background(), in).CompressedPrompt

	if strings.Contains(out, "\n\tx := 1") || strings.Contains(out, "\n\ty := 2") {
		t.Errorf("a tab-indented body survived — the pairing changed and this pin must be re-measured: %q", out)
	}
	if !strings.Contains(out, "\n x := 1") || !strings.Contains(out, "\n y := 2") {
		t.Errorf("expected BOTH fenced Go bodies detabbed, not just the first; got %q", out)
	}

	// And the regions the regex did protect are the prose gaps, named so the
	// inversion is stated rather than implied.
	matched := codeBlockRE.FindAllString(in, -1)
	if len(matched) != 2 {
		t.Fatalf("expected 2 matched regions, got %d: %q", len(matched), matched)
	}
	if !strings.Contains(matched[0], "to fence.") || !strings.Contains(matched[1], "\nand\n") {
		t.Errorf("the protected regions should be the PROSE gaps, which is the inversion: %q", matched)
	}
}

// A README THAT CONTAINS A FENCED EXAMPLE IS THE SAME BUG WITH AN EVEN NUMBER OF
// RUNS — so parity alone does not describe it. The inner python sits outside every
// matched pair and collapses inside the fence the caller wrote.
func TestFencePairing_NestedFencesLeaveTheInnerCodeOutsideEveryPair(t *testing.T) {
	const in = "Fix my README:\n```markdown\n# Title\n\n```python\ndef f():\n    if x:\n        return 1\n```\n\nDone.\n```\n"
	if n := strings.Count(in, "```"); n != 4 {
		t.Fatalf("premise: this input must carry an EVEN number of runs, got %d", n)
	}
	out := New().Compress(context.Background(), in).CompressedPrompt

	if strings.Contains(out, "\n    if x:\n        return 1") {
		t.Errorf("the nested python survived — the pairing changed: %q", out)
	}
	if !strings.Contains(out, "\n if x:\n return 1") {
		t.Errorf("expected the nested python flattened; got %q", out)
	}
}

// AN OPENED-BUT-UNCLOSED FENCE PROTECTS NOTHING. A paste truncated by a context
// limit or a tool's output cap is the ordinary way this arrives, and what it costs
// is the column alignment that makes a log readable.
func TestFencePairing_AnUnterminatedFenceProtectsNothing(t *testing.T) {
	const in = "Here is the log:\n```\n2026-01-01  ERROR   boom\n2026-01-01  INFO    ok\n"
	out := New().Compress(context.Background(), in).CompressedPrompt

	if strings.Contains(out, "2026-01-01  ERROR") {
		t.Errorf("the log's column alignment survived — the pairing changed: %q", out)
	}
	if !strings.Contains(out, "2026-01-01 ERROR boom") {
		t.Errorf("expected the aligned columns collapsed to single spaces; got %q", out)
	}
	if codeBlockRE.MatchString(in) {
		t.Errorf("a lone ``` run must match nothing, so nothing is protected: %q", codeBlockRE.FindAllString(in, -1))
	}
}

// TWO MARKDOWN CODE FORMS THIS REWRITER DOES NOT KNOW ARE CODE. Both are in the
// corpus for that reason, and both carry no ``` at all — which is why the census
// counts fenced coverage at 15 of 17 rather than claiming them.
func TestFencePairing_TildeFencesAndIndentedBlocksAreNotCodeHere(t *testing.T) {
	c := New()
	ctx := context.Background()

	const tilde = "Review:\n~~~python\ndef f():\n    if x:\n        return 1\n~~~\n"
	if out := c.Compress(ctx, tilde).CompressedPrompt; !strings.Contains(out, "\n if x:\n return 1") {
		t.Errorf("~~~ fence: expected the python flattened (CommonMark calls this a fence; this rewriter does not); got %q", out)
	}

	const indented = "Review:\n\n    def f():\n        if x:\n            return 1\n\nthanks"
	if out := c.Compress(ctx, indented).CompressedPrompt; !strings.Contains(out, "\n def f():\n if x:\n return 1") {
		t.Errorf("four-space block: expected the python flattened (the oldest markdown code form); got %q", out)
	}
}

// THE ASYMMETRY, WHICH IS SMALLER BUT IS THE ONE THAT SILENTLY BREAKS RENDERING.
// A fence indented inside a list item has its OPENING indentation collapsed —
// that run of spaces is outside the match — while the closing fence keeps its
// three, because that one IS inside. The block stops being a child of the list
// item and the two fences stop agreeing.
func TestFencePairing_TheOpeningFenceIndentIsCollapsedButTheClosingOneIsNot(t *testing.T) {
	const in = "Steps:\n\n1. run this:\n\n   ```sh\n   make  test\n   ```\n\n2. done\n"
	out := New().Compress(context.Background(), in).CompressedPrompt

	if strings.Contains(out, "\n   ```sh") {
		t.Errorf("the opening fence kept its list indentation — the boundary moved: %q", out)
	}
	if !strings.Contains(out, "\n ```sh") {
		t.Errorf("expected the opening fence's three spaces collapsed to one; got %q", out)
	}
	if !strings.Contains(out, "\n   ```\n") {
		t.Errorf("expected the CLOSING fence to keep its three spaces, which is the asymmetry: %q", out)
	}
	// The body keeps its indentation because it is inside the match — so the block
	// is now indented deeper than its own opening fence.
	if !strings.Contains(out, "\n   make  test") {
		t.Errorf("expected the body's indentation and double space preserved inside the match; got %q", out)
	}
}
