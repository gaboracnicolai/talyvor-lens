package compressor

import (
	"context"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/poolsafety"
)

// WHAT THIS FILE IS FOR.
//
// The rewriter in this package shipped on every non-streaming request for its
// whole life. internal/workspace/compression_policy.go now defaults it OFF, and
// the argument for that default is a measurement: it saves nothing on the corpora
// this repo owns, and it silently changes content on the one corpus that models
// the traffic Lens actually serves. This file IS that measurement, run against the
// SHIPPED code (same package — no copy to drift), so re-enabling it has to argue
// with numbers rather than with a description.
//
// ⚠ IT COMPARES STRINGS, NEVER SavingsPct. A prompt can be modified while
// SavingsPct reads 0.00% (len/4 integer division) — TestSavings_ZeroDoesNotMean
// Untouched below is the proof. Any audit written as "only look where savings > 0"
// is blind to exactly the class this gate exists for.

// modifiedCount is the ONE counting loop every assertion here goes through,
// including the positive control that splices a compressible prompt INTO the
// corpus. A control that ran beside the loop instead of through it would prove
// nothing about the loop.
func modifiedCount(prompts []string) (modified int, firstName string) {
	c := New()
	ctx := context.Background()
	for _, p := range prompts {
		if out := c.Compress(ctx, p).CompressedPrompt; out != p {
			modified++
			if firstName == "" {
				firstName = p
			}
		}
	}
	return modified, firstName
}

func pairPrompts(pairs []poolsafety.RephrasePair) []string {
	out := make([]string, 0, len(pairs)*2)
	for _, p := range pairs {
		out = append(out, p.A, p.B)
	}
	return out
}

// corpusSet is one committed corpus, with its prompt count PINNED. The count is
// pinned because a source-derived guard cannot see a deletion: a corpus that
// shrank to two prompts would still report "0 modified" and read as reassuring.
type corpusSet struct {
	name        string
	prompts     []string
	wantPrompts int
	wantModify  int
	why         string
}

func filler0Corpora() []corpusSet {
	return []corpusSet{
		{"engineering rephrase", pairPrompts(poolsafety.EngineeringRephrasePairs()), 60, 0, ""},
		{"engineering danger", pairPrompts(poolsafety.EngineeringDangerPairs()), 48, 0, ""},
		{"engineering held-out danger", pairPrompts(poolsafety.HeldOutDangerPairs()), 40, 0, ""},
		{"consumer rephrase", pairPrompts(poolsafety.ConsumerRephrasePairs()), 20, 0, ""},
		{"consumer danger", pairPrompts(poolsafety.ConsumerDangerPairs()), 64, 0, ""},
		{"consumer unrelated", pairPrompts(poolsafety.ConsumerUnrelatedPairs()), 20, 0, ""},
		{"rephrase (consumer-style)", pairPrompts(poolsafety.RephrasePairs()), 56, 0, ""},
	}
}

// THE HEADLINE NUMBER, PINNED PER SET AND IN TOTAL: 308 prompts, 0 modified.
//
// Per-set as well as total, because a total alone would let one set go to zero
// prompts while another grew — the sum would hold and the coverage would not.
func TestReach_TheRewriterModifiesNothingInThe308PromptCorpora(t *testing.T) {
	total := 0
	for _, s := range filler0Corpora() {
		if len(s.prompts) != s.wantPrompts {
			t.Errorf("%s: %d prompts, pinned at %d — the corpus moved, so this set's zero is no longer the zero that was measured", s.name, len(s.prompts), s.wantPrompts)
		}
		total += len(s.prompts)
		if got, first := modifiedCount(s.prompts); got != s.wantModify {
			t.Errorf("%s: %d of %d prompts modified, want %d; first was %q", s.name, got, len(s.prompts), s.wantModify, first)
		}
	}
	if total != 308 {
		t.Errorf("the corpora total %d prompts, pinned at 308", total)
	}
}

// THE POSITIVE CONTROL, SPLICED INTO THE CORPUS PATH. Take the real corpus, add
// ONE compressible prompt, and the same loop must report exactly one hit. Without
// this, "0 modified" is indistinguishable from a loop that reads nothing.
func TestReach_TheCountingLoopCanRegisterAHit(t *testing.T) {
	real := pairPrompts(poolsafety.RephrasePairs())
	if got, _ := modifiedCount(real); got != 0 {
		t.Fatalf("premise: the real corpus must start at 0 modified, got %d", got)
	}
	spiked := append(append([]string(nil), real...), "Please  rewrite this in order to be shorter.")
	got, first := modifiedCount(spiked)
	if got != 1 {
		t.Fatalf("splicing one compressible prompt into the corpus must report exactly 1 modification, got %d", got)
	}
	if !strings.Contains(first, "Please") {
		t.Errorf("the reported modification is not the spliced prompt: %q", first)
	}
	// And back to zero on the real corpus — the zero is the corpus, not the loop.
	if got, _ := modifiedCount(real); got != 0 {
		t.Errorf("the real corpus must return to 0 modified, got %d", got)
	}
}

// THE CORPUS THE 0.000% MEASUREMENT DID NOT RUN, AND IT IS THE ONE THAT MATTERS.
//
// poolsafety.Corpus() is this repo's stand-in for real coding-agent traffic: a
// system preamble followed by an UNFENCED code diff or snippet, which is what an
// agent actually posts. Every prompt in it is rewritten — 8 of 8 — because the
// space-run collapse ([ \t]+ -> " ") applies to all text outside a ``` fence,
// including leading indentation.
func TestReach_EveryAgentTrafficPromptIsRewritten(t *testing.T) {
	var prompts []string
	for _, p := range poolsafety.Corpus() {
		prompts = append(prompts, p.Full(p.A), p.Full(p.B))
	}
	if len(prompts) != 8 {
		t.Fatalf("poolsafety.Corpus() renders %d prompts, pinned at 8", len(prompts))
	}
	got, _ := modifiedCount(prompts)
	if got != 8 {
		t.Errorf("%d of 8 agent-traffic prompts modified, pinned at 8 — if the rewriter stopped touching them, the compression gate's argument needs re-measuring, not this number editing", got)
	}
}

// AND THE SPECIFIC LOSS, NAMED. A tab-indented Go line inside a diff arrives
// space-indented; the diff marker and the code are no longer separable by column.
func TestReach_LeadingIndentationIsCollapsedOutsideAFence(t *testing.T) {
	c := New()
	in := poolsafety.Corpus()[0].Full(poolsafety.Corpus()[0].A)
	if !strings.Contains(in, "\n-\tif acct.Balance") {
		t.Fatalf("premise: the corpus payload no longer carries the tab-indented diff line this pins")
	}
	out := c.Compress(context.Background(), in).CompressedPrompt
	if strings.Contains(out, "\n-\tif acct.Balance") {
		t.Errorf("the tab-indented diff line survived — re-measure the gate's argument")
	}
	if !strings.Contains(out, "\n- if acct.Balance") {
		t.Errorf("expected the tab run collapsed to one space; got %q", out)
	}
}

// PYTHON IS THE WORST CASE AND IT IS NOT HYPOTHETICAL: indentation is the syntax.
// Two nesting levels collapse to the SAME level, so the program the provider is
// asked about is not the program the caller sent. In THIS prompt the ``` fence is
// the boundary for indentation — fenced code keeps its indentation, unfenced does
// not.
//
// ⚠ AND THIS COMMENT HAS NOW OVERSTATED THE FENCE TWICE, EACH TIME IN A WAY A
// CORPUS WOULD HAVE CAUGHT. It first said the fence was the boundary, full stop;
// that was narrowed to "for indentation" when compressCodeBlock turned out to
// delete blank lines and right-trim inside the fence, changing the value of a
// multi-line literal (fence_reach_test.go). It is now narrowed again, and this is
// the larger of the two: the fence is the boundary for indentation ONLY WHEN THE
// PAIRING IS RIGHT. codeBlockRE pairs backtick runs in document order with no
// notion of which one opens a block, so a single stray ``` earlier in the prompt —
// a sentence that mentions one, a README containing one, a truncated paste —
// swaps the protected and unprotected regions for the whole remainder, and the
// python below collapses INSIDE a fence the caller wrote. Measured and pinned in
// fence_pairing_test.go against the corpus in fence_corpus_test.go, which is the
// population that had to exist first: 0 of the previous 316 corpus prompts
// contained a fence at all, so none of the three narrowings could have come from
// a measurement rather than from someone happening to look.
func TestReach_UnfencedPythonNestingCollapsesButFencedSurvives(t *testing.T) {
	c := New()
	ctx := context.Background()
	const py = "def f(vol):\n    if vol.ndim != 3:\n        raise ValueError('rank')\n    return vol.sum()"

	unfenced := c.Compress(ctx, "Fix this:\n"+py).CompressedPrompt
	if strings.Contains(unfenced, "        raise") {
		t.Error("unfenced: the inner nesting level survived — re-measure the gate's argument")
	}
	if !strings.Contains(unfenced, "\n raise ValueError('rank')") || !strings.Contains(unfenced, "\n if vol.ndim") {
		t.Errorf("unfenced: expected both levels flattened to one space; got %q", unfenced)
	}

	fenced := c.Compress(ctx, "Fix this:\n```python\n"+py+"\n```").CompressedPrompt
	if !strings.Contains(fenced, "        raise ValueError('rank')") {
		t.Errorf("fenced: indentation inside a fence must be preserved; got %q", fenced)
	}
}

// THE FOUR CONTENT-CORRUPTION CASES, EACH BY NAME AND WITH ITS REASON. These are
// pinned as KNOWN BEHAVIOUR of a gated-off feature. A future re-enable has to
// argue with them; a future FIX has to delete them deliberately.
func TestReach_TheFourContentCorruptionCases(t *testing.T) {
	c := New()
	ctx := context.Background()
	cases := []struct {
		name, in, want, why string
	}{
		{
			name: "the question becomes unanswerable",
			in:   "Explain when to use 'in order to' versus 'to' in formal writing.",
			want: "Explain when to use 'to' versus 'to' in formal writing.",
			why:  "the two things being contrasted are now the same thing; nothing logs why the answer is nonsense",
		},
		{
			name: "translate-exactly is not translated exactly",
			in:   `Translate to French, exactly: "Could you please send the report in order to close the ticket."`,
			want: `Translate to French, exactly: "send the report to close the ticket."`,
			why:  "the instruction says exactly, and the payload was edited before the model saw it",
		},
		{
			name: "a string literal the user asked to fix is rewritten",
			in:   `Fix the typo in this string literal: msg = "Please retry in order to continue"`,
			want: `Fix the typo in this string literal: msg = "retry to continue"`,
			why:  "the literal IS the subject; an unfenced string is indistinguishable from prose to a regex list",
		},
		{
			name: "the politeness comparison compares a string with itself",
			in:   "Rewrite this email to be more polite: 'Send me the file.' Compare it to 'Could you please send me the file.'",
			want: "Rewrite this email to be more polite: 'Send me the file.' Compare it to 'send me the file.'",
			why:  "the polite variant is the thing being deleted, so the comparison has no second side",
		},
	}
	for _, tc := range cases {
		got := c.Compress(ctx, tc.in).CompressedPrompt
		if got != tc.want {
			t.Errorf("%s: rewrite changed\n got  %q\n want %q\n (%s)", tc.name, got, tc.want, tc.why)
		}
		if got == tc.in {
			t.Errorf("%s: this case is no longer a corruption at all — if the rewriter was fixed, delete this case and re-measure the gate's argument (%s)", tc.name, tc.why)
		}
	}
}

// THE TRAP FOR ANY AUDIT BOUNDED BY THE SAVINGS NUMBER. A blank line removed
// inside a fenced Python block changes the bytes sent upstream while SavingsPct
// reads exactly 0.00%, because savings are computed on len/4 integer division.
// "savings == 0" does not mean "untouched".
func TestSavings_ZeroDoesNotMeanUntouched(t *testing.T) {
	c := New()
	in := "```python\nx = 1\n\ny = 2\n```"
	res := c.Compress(context.Background(), in)
	if res.CompressedPrompt == in {
		t.Fatalf("premise: this input must be modified; got an identical string")
	}
	if res.SavingsPct != 0 {
		t.Errorf("SavingsPct = %v, want exactly 0 — the whole point is a modification the percentage cannot see", res.SavingsPct)
	}
}

// The poolsafety positive-control set is the one place a filler/whitespace rule
// does fire in the rephrase corpora, and it fires for a reason worth naming: the
// 'whitespace' control's B side differs from its A side ONLY by a trailing space,
// which the final TrimSpace removes. Pinned so the 0-modified sets above cannot be
// explained away as "the loop never fires on poolsafety data".
func TestReach_TheTrailingSpaceControlIsTheOneHit(t *testing.T) {
	prompts := pairPrompts(poolsafety.PositiveControls())
	if len(prompts) != 6 {
		t.Fatalf("PositiveControls renders %d prompts, pinned at 6", len(prompts))
	}
	got, first := modifiedCount(prompts)
	if got != 1 {
		t.Errorf("%d of 6 positive-control prompts modified, pinned at 1", got)
	}
	if !strings.HasSuffix(first, " ") {
		t.Errorf("the single hit should be the trailing-space control; got %q", first)
	}
}
