package cohort

import (
	"testing"

	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/worktier"
)

// GOLDEN PARITY (proof 1): DeriveInputCohort must equal the serve path's INLINE composition
// (proxy.go:1366-ish) — routingComplexityBucket = worktier.ComplexityBucketFor(router.AnalyseComplexity(
// prompt).Score()) and the input bucket = mining.InputBucketFor(len(prompt)/4). This pins the SHARED
// derivation: if a future change edits the serve path's composition without updating cohort (or vice
// versa), this turns red. Sample inputs span the input-token buckets and complexity range.
func TestDeriveInputCohort_MatchesServePathComposition(t *testing.T) {
	inputs := []string{
		"hi",
		"Summarize this short paragraph in one sentence.",
		"Write a Go function that reverses a linked list and explain the time complexity step by step, considering edge cases for empty and single-node lists.",
		largeInput(3000),  // ~ medium/large boundary
		largeInput(40000), // xlarge
		"",
	}
	for _, in := range inputs {
		gotRange, gotComplexity := DeriveInputCohort(in)

		// The serve path's EXACT inline computation, on the same (uncompressed) input.
		wantRange := mining.InputBucketFor(len(in) / 4)
		wantComplexity := string(worktier.ComplexityBucketFor(router.AnalyseComplexity(in).Score()))

		if gotRange != wantRange {
			t.Errorf("input_token_range drift for len=%d: got %q, serve-path %q", len(in), gotRange, wantRange)
		}
		if gotComplexity != wantComplexity {
			t.Errorf("complexity_bucket drift for len=%d: got %q, serve-path %q", len(in), gotComplexity, wantComplexity)
		}
	}
}

// complexity_bucket must be within the closed worktier enum (or ”) — the value-space routing_predictions
// validates against, so eval-item tags and prediction cohorts share one vocabulary.
func TestDeriveInputCohort_ComplexityInClosedEnum(t *testing.T) {
	valid := map[string]bool{"": true, "trivial": true, "simple": true, "moderate": true, "complex": true}
	for _, in := range []string{"", "a", "explain quantum error correction in depth with proofs", largeInput(9000)} {
		_, c := DeriveInputCohort(in)
		if !valid[c] {
			t.Errorf("complexity_bucket %q for len=%d not in the closed worktier enum", c, len(in))
		}
	}
}

func largeInput(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
