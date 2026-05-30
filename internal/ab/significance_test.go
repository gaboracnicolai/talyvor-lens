package ab

import (
	"context"
	"strings"
	"testing"
)

// twoVariantExp builds a minimal valid 2-variant quality experiment.
func twoVariantExp(id string) *Experiment {
	return &Experiment{
		ID:           id,
		WorkspaceID:  "ws1",
		Name:         "sig-test",
		Status:       StatusRunning,
		Metric:       MetricQuality,
		Variants:     []Variant{{ID: "A", Name: "A", Model: "m"}, {ID: "B", Name: "B", Model: "m"}},
		TrafficSplit: []float64{0.5, 0.5},
	}
}

func record(t *testing.T, e *Engine, expID, variant string, scores []float64) {
	t.Helper()
	for _, s := range scores {
		if err := e.RecordResult(context.Background(), ExperimentResult{
			ExperimentID: expID, VariantID: variant, QualityScore: s,
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
}

func constSlice(v float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// A large, unambiguous quality gap with plenty of samples must report a
// statistically SIGNIFICANT result and name the winning direction.
func TestSignificance_LargeEffectSignificant(t *testing.T) {
	e := NewEngine(nil)
	exp := twoVariantExp("exp1")
	if err := e.CreateExperiment(context.Background(), exp); err != nil {
		t.Fatalf("create: %v", err)
	}
	record(t, e, "exp1", "A", constSlice(0.9, 60))
	record(t, e, "exp1", "B", constSlice(0.3, 60))

	rep, err := e.Significance(context.Background(), "exp1")
	if err != nil {
		t.Fatalf("significance: %v", err)
	}
	if !rep.Significant {
		t.Fatalf("expected significant, got %+v", rep)
	}
	if !strings.Contains(strings.ToLower(rep.Summary), "significant") {
		t.Errorf("summary should state significance: %q", rep.Summary)
	}
	// A (0.9) should be the higher-scoring variant.
	if rep.Verdict.Direction != 1 {
		t.Errorf("direction = %d, want +1 (A>B)", rep.Verdict.Direction)
	}
}

// A tiny sample must NEVER be declared significant — the honesty guarantee.
func TestSignificance_TinySampleInconclusive(t *testing.T) {
	e := NewEngine(nil)
	exp := twoVariantExp("exp2")
	if err := e.CreateExperiment(context.Background(), exp); err != nil {
		t.Fatalf("create: %v", err)
	}
	record(t, e, "exp2", "A", []float64{0.9, 0.95})
	record(t, e, "exp2", "B", []float64{0.3, 0.35})

	rep, err := e.Significance(context.Background(), "exp2")
	if err != nil {
		t.Fatalf("significance: %v", err)
	}
	if rep.Significant {
		t.Fatalf("tiny sample must be inconclusive, got significant: %+v", rep)
	}
	if !strings.Contains(strings.ToLower(rep.Summary), "not enough data") {
		t.Errorf("summary should flag insufficient data: %q", rep.Summary)
	}
}

// Two near-identical variants with adequate n must report "not significant"
// (inconclusive), never a winner.
func TestSignificance_NoRealDifference(t *testing.T) {
	e := NewEngine(nil)
	exp := twoVariantExp("exp3")
	if err := e.CreateExperiment(context.Background(), exp); err != nil {
		t.Fatalf("create: %v", err)
	}
	a := []float64{0.50, 0.60, 0.55, 0.52, 0.58, 0.51, 0.49, 0.57, 0.53, 0.54}
	b := []float64{0.51, 0.59, 0.54, 0.53, 0.57, 0.50, 0.52, 0.56, 0.55, 0.53}
	record(t, e, "exp3", "A", a)
	record(t, e, "exp3", "B", b)

	rep, err := e.Significance(context.Background(), "exp3")
	if err != nil {
		t.Fatalf("significance: %v", err)
	}
	if rep.Significant {
		t.Fatalf("near-identical variants must not be significant: %+v", rep)
	}
	if rep.Verdict.PValue < 0.05 {
		t.Errorf("p = %.4f, want >= 0.05", rep.Verdict.PValue)
	}
}

// With fewer than two variants carrying data, there's nothing to compare.
func TestSignificance_InsufficientVariants(t *testing.T) {
	e := NewEngine(nil)
	exp := twoVariantExp("exp4")
	if err := e.CreateExperiment(context.Background(), exp); err != nil {
		t.Fatalf("create: %v", err)
	}
	record(t, e, "exp4", "A", constSlice(0.8, 20))

	rep, err := e.Significance(context.Background(), "exp4")
	if err != nil {
		t.Fatalf("significance: %v", err)
	}
	if rep.Significant {
		t.Fatalf("only one variant has data — cannot be significant: %+v", rep)
	}
	if !strings.Contains(strings.ToLower(rep.Summary), "inconclusive") &&
		!strings.Contains(strings.ToLower(rep.Summary), "not enough") {
		t.Errorf("summary should be inconclusive: %q", rep.Summary)
	}
}
