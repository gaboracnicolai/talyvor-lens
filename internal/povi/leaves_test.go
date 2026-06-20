package povi_test

import (
	"bytes"
	"testing"

	"github.com/talyvor/lens/internal/povi"
)

// TestStepsFromTokens_OneLeafPerToken — the core per-token property: N tokens yield
// exactly N leaves, even when tokens span multiple runes. This is what the rune
// stand-in cannot do (it would yield rune-count leaves for the same text).
func TestStepsFromTokens_OneLeafPerToken(t *testing.T) {
	tokens := []string{"Hel", "lo", ", ", "wörld", "!"} // 5 tokens, 13 runes
	steps := povi.StepsFromTokens(tokens)

	if len(steps) != len(tokens) {
		t.Fatalf("token leaves = %d, want %d (one per token)", len(steps), len(tokens))
	}

	// Contrast against the rune stand-in over the SAME text.
	joined := ""
	for _, tk := range tokens {
		joined += tk
	}
	runeSteps := povi.StepsFromRunes(joined)
	if want := len([]rune(joined)); len(runeSteps) != want {
		t.Fatalf("rune leaves = %d, want %d", len(runeSteps), want)
	}
	if len(steps) == len(runeSteps) {
		t.Fatalf("per-token (%d) must DIFFER from per-rune (%d) for multi-rune tokens — the bug is leaves-not-per-token", len(steps), len(runeSteps))
	}

	for i, tk := range tokens {
		if !bytes.Equal(steps[i], []byte(tk)) {
			t.Errorf("leaf %d = %q, want the exact token %q", i, steps[i], tk)
		}
	}
}

// TestTokenLeaves_MerkleRootVerifies — a token trace folds a root and every token
// leaf round-trips through BuildProof/VerifyPath; a tampered leaf must not verify.
func TestTokenLeaves_MerkleRootVerifies(t *testing.T) {
	tokens := []string{"the", " quick", " brown", " fox", " jumps", " over"}
	steps := povi.StepsFromTokens(tokens)
	root := povi.MerkleRoot(steps)

	for i := range steps {
		proof, err := povi.BuildProof(steps, i)
		if err != nil {
			t.Fatalf("BuildProof(%d): %v", i, err)
		}
		if !povi.VerifyPath(root, steps[i], proof) {
			t.Errorf("token leaf %d failed VerifyPath", i)
		}
	}

	proof, _ := povi.BuildProof(steps, 2)
	if povi.VerifyPath(root, []byte(" BROWN"), proof) {
		t.Error("tampered token leaf verified against the root — must fail")
	}
}
