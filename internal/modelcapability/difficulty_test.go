package modelcapability

import (
	"testing"

	"github.com/talyvor/lens/internal/worktier"
)

// TestDifficulty_BindsToH1Buckets — the capability curve's x-axis (difficulty) is
// derived ENTIRELY from H1's work-classification: the two HARDNESS axes (size +
// complexity). Cost ($) and sensitivity (privacy) are NOT difficulty and never
// enter. This is the explicit "H2 consumes H1's tier output" binding.
func TestDifficulty_BindsToH1Buckets(t *testing.T) {
	for _, c := range []struct {
		size worktier.SizeBucket
		cx   worktier.Complexity
		want int
	}{
		{worktier.SizeSmall, worktier.ComplexityTrivial, 0},
		{worktier.SizeSmall, worktier.ComplexitySimple, 1},
		{worktier.SizeMedium, worktier.ComplexitySimple, 2},
		{worktier.SizeMedium, worktier.ComplexityModerate, 3},
		{worktier.SizeLarge, worktier.ComplexityModerate, 4},
		{worktier.SizeLarge, worktier.ComplexityComplex, 5},
		{worktier.SizeXLarge, worktier.ComplexityComplex, 6},
		{worktier.SizeSmall, worktier.ComplexityComplex, 3},  // low size, high complexity
		{worktier.SizeXLarge, worktier.ComplexityTrivial, 3}, // high size, low complexity
	} {
		if got := Difficulty(c.size, c.cx); got != c.want {
			t.Errorf("Difficulty(%s,%s) = %d, want %d", c.size, c.cx, got, c.want)
		}
	}
}

// TestDifficulty_MonotonicInBothAxes — raising EITHER hardness axis never lowers
// difficulty (a curve x-axis must be a well-ordered "work rises" signal).
func TestDifficulty_MonotonicInBothAxes(t *testing.T) {
	sizes := []worktier.SizeBucket{worktier.SizeSmall, worktier.SizeMedium, worktier.SizeLarge, worktier.SizeXLarge}
	cxs := []worktier.Complexity{worktier.ComplexityTrivial, worktier.ComplexitySimple, worktier.ComplexityModerate, worktier.ComplexityComplex}
	for si := 1; si < len(sizes); si++ {
		for _, cx := range cxs {
			if Difficulty(sizes[si], cx) < Difficulty(sizes[si-1], cx) {
				t.Errorf("raising size %s→%s lowered difficulty at complexity %s", sizes[si-1], sizes[si], cx)
			}
		}
	}
	for ci := 1; ci < len(cxs); ci++ {
		for _, sz := range sizes {
			if Difficulty(sz, cxs[ci]) < Difficulty(sz, cxs[ci-1]) {
				t.Errorf("raising complexity %s→%s lowered difficulty at size %s", cxs[ci-1], cxs[ci], sz)
			}
		}
	}
}

// TestDifficultyOf_FromH1TierOutputs — the two convenience entries take H1's
// actual output types (the pre-serve projection AND the persisted WorkTier) and
// agree with the axis-level Difficulty. Proves H2 consumes H1's classification
// objects directly, not a re-derivation.
func TestDifficultyOf_FromH1TierOutputs(t *testing.T) {
	a := worktier.NewAdvisor()
	pre := a.Project(worktier.PreServeSignals{InputTokens: 50000, ComplexityScore: 5}) // large + complex
	if got, want := DifficultyOf(pre), Difficulty(pre.Size, pre.Complexity); got != want {
		t.Errorf("DifficultyOf(pre-serve) = %d, want %d", got, want)
	}
	if DifficultyOf(pre) != 5 {
		t.Errorf("large+complex pre-serve tier difficulty = %d, want 5", DifficultyOf(pre))
	}
	wt := worktier.Classify(120000, 2000, 0.5, 5, false, false, "full") // xlarge + complex
	if got, want := DifficultyOfWorkTier(wt), Difficulty(wt.Size, wt.Complexity); got != want {
		t.Errorf("DifficultyOfWorkTier = %d, want %d", got, want)
	}
	if DifficultyOfWorkTier(wt) != 6 {
		t.Errorf("xlarge+complex WorkTier difficulty = %d, want 6", DifficultyOfWorkTier(wt))
	}
}

// TestMaxDifficulty — the curve x-axis spans a known, fixed range [0, MaxDifficulty].
func TestMaxDifficulty(t *testing.T) {
	if MaxDifficulty != 6 {
		t.Errorf("MaxDifficulty = %d, want 6 (size 0..3 + complexity 0..3)", MaxDifficulty)
	}
	if got := Difficulty(worktier.SizeXLarge, worktier.ComplexityComplex); got != MaxDifficulty {
		t.Errorf("max shape difficulty = %d, want MaxDifficulty %d", got, MaxDifficulty)
	}
}
