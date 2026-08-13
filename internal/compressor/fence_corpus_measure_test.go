package compressor

import (
	"context"
	"reflect"
	"testing"
)

// THE MEASUREMENT, THROUGH THE SAME Compress THE PROXY CALLS.
//
// Every prompt's OUTPUT BYTES are pinned, plus the techniques the rewriter
// credited itself with. Both, per prompt, because each is blind to the other: C34
// moves the attribution without changing a byte, and C27 changes the bytes a
// provider would receive while leaving the attribution — and a "was it modified"
// boolean — exactly as they were.
//
// ⚠ THE BOOLEAN WAS THE FIRST DRAFT AND IT WAS CAUGHT BLIND. See fencedPrompt's
// doc: pinning "modified: true" passed happily while a destroyed python block
// turned back into an intact one. Nothing here pins an opinion about what the
// rewriter SHOULD do; it pins what it DOES, so that changing it is deliberate.
//
// Controls: C27-C29 and C34 of w61-fencecorpus-controls-9d72.py move the bytes or
// the attribution; C30 shrinks the corpus underneath both pins, which is the one
// mutation this test CANNOT see and the reason the aggregate below exists.
func TestFenceCorpus_EveryPromptsOutcomeIsPinned(t *testing.T) {
	c := New()
	ctx := context.Background()
	for _, p := range fencedCorpus() {
		res := c.Compress(ctx, p.in)
		if res.CompressedPrompt != p.wantOut {
			t.Errorf("%s: output bytes moved (%s)\n  in   %q\n  got  %q\n  want %q",
				p.name, p.why, p.in, res.CompressedPrompt, p.wantOut)
		}
		if !reflect.DeepEqual(res.TechniquesApplied, p.wantTechniques) {
			t.Errorf("%s: techniques=%v, pinned %v (%s)\n  out %q",
				p.name, res.TechniquesApplied, p.wantTechniques, p.why, res.CompressedPrompt)
		}
	}
}

// AND THE AGGREGATE, WHICH THE PER-PROMPT PINS CANNOT REPLACE: how much of this
// corpus the rewriter touches at all. Pinned so that a corpus curated over time
// into "cases the rewriter leaves alone" — the comfortable direction for a
// corpus to drift — has to say so out loud.
func TestFenceCorpus_FourteenOfSeventeenAreRewritten(t *testing.T) {
	corpus := fencedCorpus()
	if len(corpus) != 17 {
		t.Fatalf("the fenced corpus is %d prompts, pinned at 17", len(corpus))
	}
	c := New()
	ctx := context.Background()
	modified := 0
	for _, p := range corpus {
		if c.Compress(ctx, p.in).CompressedPrompt != p.in {
			modified++
		}
	}
	if modified != 14 {
		t.Errorf("%d of 17 fenced prompts are rewritten, pinned at 14 — the three untouched ones are the "+
			"clean go block, the unified diff and the clean python block, i.e. the technique behaving as "+
			"advertised. A move here is the headline, not a stale constant", modified)
	}
}
