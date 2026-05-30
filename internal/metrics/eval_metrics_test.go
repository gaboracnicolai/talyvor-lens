package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The eval-run result label must be a BOUNDED set. Any unexpected value folds
// to "unknown" so a buggy caller can't explode cardinality.
func TestEvalRunRecorded_CardinalitySafeLabel(t *testing.T) {
	EvalRunRecorded("pass")
	EvalRunRecorded("fail")
	EvalRunRecorded("nonsense-unbounded-value")

	if v := testutil.ToFloat64(EvalRunsTotal.WithLabelValues("pass")); v < 1 {
		t.Errorf("pass counter = %v, want >= 1", v)
	}
	if v := testutil.ToFloat64(EvalRunsTotal.WithLabelValues("unknown")); v < 1 {
		t.Errorf("unbounded value should fold to 'unknown', got %v", v)
	}
}

func TestEvalRegressionsAndSignificance(t *testing.T) {
	before := testutil.ToFloat64(EvalRegressionsDetectedTotal)
	EvalRegressionsDetected(3)
	EvalRegressionsDetected(0) // no-op
	if got := testutil.ToFloat64(EvalRegressionsDetectedTotal); got != before+3 {
		t.Errorf("regressions counter = %v, want %v", got, before+3)
	}

	b2 := testutil.ToFloat64(ABSignificantResultsTotal)
	ABSignificantResult()
	if got := testutil.ToFloat64(ABSignificantResultsTotal); got != b2+1 {
		t.Errorf("ab significant counter = %v, want %v", got, b2+1)
	}
}
