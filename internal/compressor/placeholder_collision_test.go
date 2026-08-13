package compressor

import (
	"context"
	"strings"
	"testing"
)

// THE CODE-BLOCK PLACEHOLDER IS IN-BAND SIGNALLING, AND THE BAND IS THE CUSTOMER'S
// PROMPT.
//
// Compress lifts every fenced block out of the prompt and leaves "\x00CODE<n>\x00"
// behind, applies the text techniques to what remains, then splices each block back
// with strings.Replace(text, placeholder, compressed, 1) — the FIRST occurrence.
// Nothing establishes that the first occurrence is the marker Compress itself wrote.
// A prompt that already contains those bytes owns the first occurrence, and the
// splice lands on the caller's text instead of on the marker.
//
// ⚠ MEASURED THROUGH THE WIRE BEFORE IT WAS WRITTEN DOWN. With the 0117 gate open,
// this prompt:
//
//	"before \x00CODE0\x00 after\n```python\nx = 1\n\ny = 2\n```\n"
//
// reached the provider as:
//
//	"before ```python\nx = 1\ny = 2\n``` after\n\x00CODE0\x00"
//
// Two separate failures in one 200 response. The fenced block MOVED — from the end
// of the prompt into the middle, ahead of the word "after" that used to precede it,
// which is a change of meaning and not a compression. And the gateway's own internal
// marker, NUL bytes included, was transmitted to a third party as if it were the
// customer's content. The request succeeded; the token accounting recorded a
// two-byte saving.
//
// ⚠ WHY A NUL IS REACHABLE AND NOT HYPOTHETICAL. The prompt arrives as a JSON string
// and encoding/json turns a backslash-u-0000 escape into a real NUL byte, so any
// caller can put one in a
// prompt. So can an agent pasting a log, a hexdump, or the output of a tool that
// emits NUL separators — this does not need an attacker, only a byte.
//
// ⚠ THE FIX IS TO REFUSE, NOT TO ESCAPE. Phase 1 of the design is LOSSLESS ONLY, so
// the honest answer to "I cannot represent this prompt in my own encoding" is to
// hand back the caller's bytes untouched. Escaping would mean inventing a second
// encoding on a money path to protect the first one, and a rewriter that returns the
// original is a rewriter that cannot corrupt. It costs the saving on prompts
// containing NUL, which is a saving this rewriter measured at 0.000% over 308 corpus
// prompts anyway.
//
// The three tests below are red on the pre-fix compressor: the first two on the
// corrupted output, the third on TechniquesApplied claiming work it must not do.
// Their positive control is w61-placeholder-controls-4f2b.py.

// The exact prompt measured through the proxy. Kept as one constant so the wire
// test in internal/proxy and this unit test cannot drift onto different inputs.
const forgedPlaceholderPrompt = "before \x00CODE0\x00 after\n```python\nx = 1\n\ny = 2\n```\n"

func TestCompress_AForgedPlaceholderLeavesThePromptByteIdentical(t *testing.T) {
	res := New().Compress(context.Background(), forgedPlaceholderPrompt)

	if res.CompressedPrompt != forgedPlaceholderPrompt {
		t.Errorf("a prompt carrying the compressor's own placeholder was rewritten.\n got %q\nwant %q",
			res.CompressedPrompt, forgedPlaceholderPrompt)
	}
}

// The half that says WHY the byte-identity above matters: without it the fenced
// block is relocated and the marker is transmitted. Asserted separately so a
// future change that fixes one and not the other cannot hide behind a single
// equality.
func TestCompress_TheInternalMarkerIsNeverHandedBack(t *testing.T) {
	res := New().Compress(context.Background(), forgedPlaceholderPrompt)

	// The marker in the INPUT is the caller's own bytes and must survive verbatim;
	// what must not happen is the code block moving out from behind it.
	if !strings.HasPrefix(res.CompressedPrompt, "before \x00CODE0\x00 after") {
		t.Errorf("the fenced block was relocated past the caller's text: %q", res.CompressedPrompt)
	}
	if strings.Contains(res.CompressedPrompt, "```python") &&
		strings.Index(res.CompressedPrompt, "```python") < strings.Index(res.CompressedPrompt, "after") {
		t.Errorf("the code block now precedes the word that followed it: %q", res.CompressedPrompt)
	}
}

// A refusal must not be reported as work. TechniquesApplied feeds the technique
// attribution the design calls the whole pitch, and a rewriter that returns the
// original while naming "code_blocks" and "whitespace" would be crediting itself
// for a rewrite it correctly declined to make.
func TestCompress_ARefusedPromptClaimsNoTechniquesAndNoSaving(t *testing.T) {
	res := New().Compress(context.Background(), forgedPlaceholderPrompt)

	if len(res.TechniquesApplied) != 0 {
		t.Errorf("TechniquesApplied = %v on a prompt that was not rewritten, want none", res.TechniquesApplied)
	}
	if res.SavingsPct != 0 {
		t.Errorf("SavingsPct = %v on a prompt that was not rewritten, want 0", res.SavingsPct)
	}
	if res.OriginalTokens != res.CompressedTokens {
		t.Errorf("token counts differ (%d vs %d) on a prompt that was not rewritten",
			res.OriginalTokens, res.CompressedTokens)
	}
}

// THE BOUNDARY, STATED AS A TEST SO IT CANNOT WIDEN BY ACCIDENT. The refusal is
// keyed on the NUL byte, which is the only character the placeholder encoding
// relies on. An ordinary prompt that merely mentions the word CODE0 is not
// special and must still be rewritten — otherwise the fix would be a silent
// kill switch triggered by content, and the measured population would shrink for
// a reason nothing records.
func TestCompress_TheRefusalIsKeyedOnTheNULAndNotOnTheWordCODE(t *testing.T) {
	const innocent = "before CODE0 after\n```python\nx = 1\n\ny = 2\n```\n"
	res := New().Compress(context.Background(), innocent)

	if res.CompressedPrompt == innocent {
		t.Error("a prompt with no NUL was refused — the refusal is triggered by content, not by encoding")
	}
	if len(res.TechniquesApplied) == 0 {
		t.Error("no techniques reported on a prompt this rewriter does modify")
	}
}
