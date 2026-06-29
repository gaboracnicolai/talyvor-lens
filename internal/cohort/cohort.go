// Package cohort derives the routing-cohort dimensions of an input string — the SAME computation the
// live serve path performs, so an eval item (PR-2) and a serve request with the same input land in the
// SAME cohort, and a routing prediction's "cohort C" (PR-1) has a resolvable held-eval slice (PR-3).
//
// It is NOT a reimplementation: it COMPOSES the identical exported primitives the serve path uses
// (proxy.go computes routingComplexityBucket = worktier.ComplexityBucketFor(router.AnalyseComplexity(
// prompt).Score()) and the input bucket via mining.InputBucketFor(len(prompt)/4)). A golden parity test
// (cohort_test.go) pins DeriveInputCohort against that inline composition so any future drift turns red.
//
// COHORT = (feature_category, input_token_range, complexity_bucket). Only the latter two are DERIVED
// here — feature_category is DECLARED at the boundary (the X-Talyvor-Feature header at serve time; the
// operator/contributor's declaration at seed time), never computed from the input.
//
// This package reaches no ledger/mint path; it only reads the input string.
package cohort

import (
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/worktier"
)

// DeriveInputCohort returns the two INPUT-DERIVED cohort dimensions for an input string, composing the
// exact exported serve-path functions:
//
//	input_token_range = mining.InputBucketFor(len(input)/4)
//	complexity_bucket = worktier.ComplexityBucketFor(router.AnalyseComplexity(input).Score())
//
// COMPRESSION RESIDUAL (a blessed, documented bound): the serve path derives on the COMPRESSED prompt
// (len(compressedPrompt)/4 and AnalyseComplexity(compressedPrompt), proxy.go), whereas an eval item is
// not compressed, so this derives on the RAW input. For short Q&A eval items compression is a no-op and
// the buckets match; a highly-compressible input could bucket differently. Accepted for PR-2 — eval
// items are concise benchmark questions.
func DeriveInputCohort(input string) (inputTokenRange, complexityBucket string) {
	inputTokenRange = mining.InputBucketFor(len(input) / 4)
	complexityBucket = string(worktier.ComplexityBucketFor(router.AnalyseComplexity(input).Score()))
	return inputTokenRange, complexityBucket
}
