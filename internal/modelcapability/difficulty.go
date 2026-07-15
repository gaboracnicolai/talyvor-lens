// Package modelcapability learns, PER MODEL, how quality holds as work-tier rises
// — a capability curve fitted across that model's own (synthetic-buildable)
// traffic. It BUILDS ON H1: the curve's x-axis (difficulty) is derived entirely
// from H1's WorkTier classification (this file), and the store below fits a
// per-model quality-vs-difficulty trend over it.
//
// DESCRIPTIVE + MINT-FREE, exactly like the WorkTier classifier it consumes: this
// package imports NO minter (pinned by the import guard), and its store holds only
// an Exec/Query handle — no Begin, no ledger — so a capability curve can never
// mint. A model's measured capability is analytics, never a reward multiplier
// (the descriptive-never-incentivized doctrine; see internal/worktier).
package modelcapability

import "github.com/talyvor/lens/internal/worktier"

// MaxDifficulty is the top of the difficulty axis: size rank 0..3 + complexity
// rank 0..3. The curve x-axis spans [0, MaxDifficulty].
const MaxDifficulty = 6

// sizeRank / complexityRank ordinalize the two HARDNESS axes of H1's tier. These
// are the only two axes that make WORK harder (bigger input, harder problem);
// cost ($) and sensitivity (privacy) are deliberately excluded from difficulty.
func sizeRank(s worktier.SizeBucket) int {
	switch s {
	case worktier.SizeXLarge:
		return 3
	case worktier.SizeLarge:
		return 2
	case worktier.SizeMedium:
		return 1
	default: // SizeSmall / unknown
		return 0
	}
}

func complexityRank(c worktier.Complexity) int {
	switch c {
	case worktier.ComplexityComplex:
		return 3
	case worktier.ComplexityModerate:
		return 2
	case worktier.ComplexitySimple:
		return 1
	default: // ComplexityTrivial / unknown
		return 0
	}
}

// Difficulty maps H1's two hardness axes to a single ordinal in [0, MaxDifficulty]
// — the capability curve's x-axis. Monotonic non-decreasing in each axis.
func Difficulty(size worktier.SizeBucket, cx worktier.Complexity) int {
	return sizeRank(size) + complexityRank(cx)
}

// DifficultyOf derives difficulty from H1's PRE-SERVE tier projection (the Advisor
// output). The H2→H1 binding on the routing-decision side.
func DifficultyOf(t worktier.PreServeTier) int { return Difficulty(t.Size, t.Complexity) }

// DifficultyOfWorkTier derives difficulty from H1's persisted post-serve WorkTier.
// The H2→H1 binding on the served-traffic side.
func DifficultyOfWorkTier(t worktier.WorkTier) int { return Difficulty(t.Size, t.Complexity) }
