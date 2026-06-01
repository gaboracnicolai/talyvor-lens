package distill

import "testing"

// ApplyTier is the exported, pure post-processing primitive the isolated path
// (ProcessIsolator returns faithful Markdown only) uses to apply a tier without
// the in-process Option machinery. It must delegate to applyTier: faithful is
// identity, outline reduces.
func TestApplyTier_ExportedDelegates(t *testing.T) {
	md := "# Title\n\nbody text that outline drops\n\n## Section\n\nmore body"
	res := Result{Markdown: md, Format: FormatHTML}

	faithful := ApplyTier(res, TierFaithful)
	if faithful.Markdown != md {
		t.Errorf("faithful must be identity; got %q", faithful.Markdown)
	}
	if faithful.Tier != TierFaithful {
		t.Errorf("faithful tier not recorded; got %q", faithful.Tier)
	}

	outline := ApplyTier(res, TierOutline)
	if outline.Tier != TierOutline {
		t.Errorf("outline tier not recorded; got %q", outline.Tier)
	}
	if len(outline.Markdown) >= len(md) {
		t.Errorf("outline should reduce the markdown; got %q", outline.Markdown)
	}
}

// ComputeSavings is the exported savings primitive (len/4 basis, the same the
// gateway bills on), for the isolated path which gets only a Result back.
func TestComputeSavings_ExportedBasis(t *testing.T) {
	in := []byte("<html><body><h1>Title</h1><p>Hello world.</p></body></html>")
	res := Result{Markdown: "# Title\n\nHello world.", Format: FormatHTML}

	s := ComputeSavings(in, res)
	if s.InputTokensRaw != len(in)/4 {
		t.Errorf("InputTokensRaw=%d, want len(in)/4=%d", s.InputTokensRaw, len(in)/4)
	}
	if s.InputTokensDistilled != len(res.Markdown)/4 {
		t.Errorf("InputTokensDistilled=%d, want len(md)/4=%d", s.InputTokensDistilled, len(res.Markdown)/4)
	}
	if s.TokensSaved != s.InputTokensRaw-s.InputTokensDistilled {
		t.Errorf("TokensSaved=%d, want raw-distilled", s.TokensSaved)
	}
	if s.CacheHit {
		t.Error("a fresh preview computation must report CacheHit=false")
	}
	if s.InputBytes != len(in) || s.OutputBytes != len(res.Markdown) {
		t.Errorf("byte counts wrong: in=%d out=%d", s.InputBytes, s.OutputBytes)
	}
}

// NeedsVision savings must be zero (no usable Markdown delivered) — the exported
// wrapper must preserve that honesty.
func TestComputeSavings_NeedsVisionZero(t *testing.T) {
	s := ComputeSavings([]byte("%PDF-1.4 ..."), Result{NeedsVision: true, Format: FormatPDF})
	if s.TokensSaved != 0 {
		t.Errorf("NeedsVision must report 0 tokens saved; got %d", s.TokensSaved)
	}
}
