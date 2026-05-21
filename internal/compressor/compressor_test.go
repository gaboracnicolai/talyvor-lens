package compressor

import (
	"context"
	"math"
	"slices"
	"testing"
)

func TestCompress_WhitespaceDeduplication(t *testing.T) {
	c := New()
	in := "  Hello   world\n\n\n\nfoo  "
	res := c.Compress(context.Background(), in)

	want := "Hello world\n\nfoo"
	if res.CompressedPrompt != want {
		t.Errorf("CompressedPrompt = %q, want %q", res.CompressedPrompt, want)
	}
	if !slices.Contains(res.TechniquesApplied, "whitespace") {
		t.Errorf("expected 'whitespace' in TechniquesApplied: %v", res.TechniquesApplied)
	}
}

func TestCompress_RedundantPhrasesCaseInsensitive(t *testing.T) {
	c := New()
	in := "Please write a poem. KINDLY include rhyme. Could You Please be brief."
	res := c.Compress(context.Background(), in)

	want := "write a poem. include rhyme. be brief."
	if res.CompressedPrompt != want {
		t.Errorf("CompressedPrompt = %q, want %q", res.CompressedPrompt, want)
	}
	if !slices.Contains(res.TechniquesApplied, "redundant_phrases") {
		t.Errorf("expected 'redundant_phrases' in TechniquesApplied: %v", res.TechniquesApplied)
	}
}

func TestCompress_CommonPatternsShortened(t *testing.T) {
	c := New()
	in := "In order to do this, as well as that, due to the fact that we must."
	res := c.Compress(context.Background(), in)

	want := "to do this, and that, because we must."
	if res.CompressedPrompt != want {
		t.Errorf("CompressedPrompt = %q, want %q", res.CompressedPrompt, want)
	}
	if !slices.Contains(res.TechniquesApplied, "common_patterns") {
		t.Errorf("expected 'common_patterns' in TechniquesApplied: %v", res.TechniquesApplied)
	}
}

func TestCompress_CodeBlockRemovesBlankLinesAndTrailingSpaces(t *testing.T) {
	c := New()
	in := "```python\nprint('a')\n\n\nprint('b')   \n```"
	res := c.Compress(context.Background(), in)

	want := "```python\nprint('a')\nprint('b')\n```"
	if res.CompressedPrompt != want {
		t.Errorf("CompressedPrompt = %q, want %q", res.CompressedPrompt, want)
	}
	if !slices.Contains(res.TechniquesApplied, "code_blocks") {
		t.Errorf("expected 'code_blocks' in TechniquesApplied: %v", res.TechniquesApplied)
	}
}

func TestCompress_EmptyPromptZeroSavings(t *testing.T) {
	c := New()
	res := c.Compress(context.Background(), "")

	if res.CompressedPrompt != "" {
		t.Errorf("CompressedPrompt = %q, want empty", res.CompressedPrompt)
	}
	if res.OriginalTokens != 0 || res.CompressedTokens != 0 {
		t.Errorf("tokens: original=%d compressed=%d, want both 0", res.OriginalTokens, res.CompressedTokens)
	}
	if res.SavingsPct != 0 {
		t.Errorf("SavingsPct = %g, want 0", res.SavingsPct)
	}
	if len(res.TechniquesApplied) != 0 {
		t.Errorf("TechniquesApplied = %v, want empty", res.TechniquesApplied)
	}
}

func TestCompress_AlreadyCompressedReturnsEmptyTechniques(t *testing.T) {
	c := New()
	res := c.Compress(context.Background(), "Hello world.")

	if res.CompressedPrompt != "Hello world." {
		t.Errorf("CompressedPrompt = %q, want %q", res.CompressedPrompt, "Hello world.")
	}
	if len(res.TechniquesApplied) != 0 {
		t.Errorf("TechniquesApplied = %v, want empty", res.TechniquesApplied)
	}
}

func TestCompress_SavingsPctCorrect(t *testing.T) {
	c := New()
	// "please write" = 12 chars → 3 tokens; "write" = 5 chars → 1 token
	// SavingsPct = (1 - 1/3) * 100 = 66.666...%
	res := c.Compress(context.Background(), "please write")

	if res.CompressedPrompt != "write" {
		t.Fatalf("CompressedPrompt = %q, want %q", res.CompressedPrompt, "write")
	}
	if res.OriginalTokens != 3 {
		t.Errorf("OriginalTokens = %d, want 3", res.OriginalTokens)
	}
	if res.CompressedTokens != 1 {
		t.Errorf("CompressedTokens = %d, want 1", res.CompressedTokens)
	}
	want := (1.0 - 1.0/3.0) * 100
	if math.Abs(res.SavingsPct-want) > 0.001 {
		t.Errorf("SavingsPct = %g, want %g", res.SavingsPct, want)
	}
}

func TestCompress_TechniquesAppliedOnlyIncludesChangedOnes(t *testing.T) {
	c := New()

	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"only whitespace", "Hello   world", []string{"whitespace"}},
		{"only redundant", "please write", []string{"redundant_phrases"}},
		{"only patterns", "in order to ship", []string{"common_patterns"}},
		{"nothing", "Hello world", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := c.Compress(context.Background(), tc.in)
			if !slices.Equal(res.TechniquesApplied, tc.want) {
				t.Errorf("TechniquesApplied = %v, want %v", res.TechniquesApplied, tc.want)
			}
		})
	}
}
