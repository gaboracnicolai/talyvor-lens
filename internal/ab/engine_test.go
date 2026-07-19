package ab

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// sampleExperiment is a 2-variant experiment that passes
// validation. Tests clone + tweak as needed.
func sampleExperiment() *Experiment {
	return &Experiment{
		WorkspaceID: "ws-1",
		Name:        "haiku vs sonnet",
		Metric:      MetricQuality,
		Variants: []Variant{
			{ID: "a", Name: "Haiku", Model: "claude-haiku-4-5", Provider: "anthropic", Weight: 0.5},
			{ID: "b", Name: "Sonnet", Model: "claude-sonnet-4-6", Provider: "anthropic", Weight: 0.5},
		},
		TrafficSplit: []float64{0.5, 0.5},
	}
}

// ─── ValidateExperiment ───────────────────────────

func TestValidate_AcceptsGoodExperiment(t *testing.T) {
	if err := ValidateExperiment(sampleExperiment()); err != nil {
		t.Fatalf("clean experiment should validate: %v", err)
	}
}

func TestValidate_RejectsFewerThanTwoVariants(t *testing.T) {
	exp := sampleExperiment()
	exp.Variants = exp.Variants[:1]
	exp.TrafficSplit = exp.TrafficSplit[:1]
	err := ValidateExperiment(exp)
	if err == nil || !strings.Contains(err.Error(), "2 variants") {
		t.Fatalf("expected ≥2 variants error, got %v", err)
	}
}

func TestValidate_TrafficSplitMustSumToOne(t *testing.T) {
	exp := sampleExperiment()
	exp.TrafficSplit = []float64{0.3, 0.3} // sums to 0.6
	err := ValidateExperiment(exp)
	if err == nil || !strings.Contains(err.Error(), "sum to 1.0") {
		t.Fatalf("expected sum error, got %v", err)
	}
}

func TestValidate_TrafficSplitToleratesSmallDrift(t *testing.T) {
	exp := sampleExperiment()
	// 0.500001 + 0.499998 = 0.999999 — within ±0.01.
	exp.TrafficSplit = []float64{0.500001, 0.499998}
	if err := ValidateExperiment(exp); err != nil {
		t.Fatalf("small drift should pass: %v", err)
	}
}

func TestValidate_TrafficSplitLengthMustMatchVariants(t *testing.T) {
	exp := sampleExperiment()
	exp.TrafficSplit = []float64{0.5, 0.3, 0.2}
	err := ValidateExperiment(exp)
	if err == nil || !strings.Contains(err.Error(), "length") {
		t.Fatalf("expected length mismatch error, got %v", err)
	}
}

// ─── AssignVariant ────────────────────────────────

func TestAssignVariant_DeterministicForSameInputs(t *testing.T) {
	exp := sampleExperiment()
	got1 := AssignVariant(exp.ID, "user-42", exp.Variants)
	got2 := AssignVariant(exp.ID, "user-42", exp.Variants)
	if got1 == nil || got2 == nil {
		t.Fatal("got nil variant")
	}
	if got1.ID != got2.ID {
		t.Fatalf("same input should pick same variant: %s vs %s", got1.ID, got2.ID)
	}
}

func TestAssignVariant_DistributesByWeight(t *testing.T) {
	variants := []Variant{
		{ID: "a", Model: "m", Weight: 0.2},
		{ID: "b", Model: "m", Weight: 0.8},
	}
	counts := map[string]int{}
	for i := 0; i < 2000; i++ {
		userID := "user-" + itoa(i)
		v := AssignVariant("exp-1", userID, variants)
		counts[v.ID]++
	}
	ratioB := float64(counts["b"]) / 2000
	// 0.8 ± 0.05 — generous band for 2000 samples of hash bucketing.
	if ratioB < 0.75 || ratioB > 0.85 {
		t.Fatalf("expected b ≈ 0.8 of traffic, got %.3f (%+v)", ratioB, counts)
	}
}

func TestAssignVariant_EmptyVariantsReturnsNil(t *testing.T) {
	if v := AssignVariant("e", "u", nil); v != nil {
		t.Fatalf("expected nil, got %+v", v)
	}
}

func TestAssignVariant_NormalisesUnnormalisedWeights(t *testing.T) {
	// Passing {50, 50} should behave the same as {0.5, 0.5}.
	variants := []Variant{
		{ID: "a", Model: "m", Weight: 50},
		{ID: "b", Model: "m", Weight: 50},
	}
	counts := map[string]int{}
	for i := 0; i < 1000; i++ {
		v := AssignVariant("exp-1", "user-"+itoa(i), variants)
		counts[v.ID]++
	}
	// Both buckets should land within 40–60%.
	for _, id := range []string{"a", "b"} {
		ratio := float64(counts[id]) / 1000
		if ratio < 0.4 || ratio > 0.6 {
			t.Errorf("%s ratio %.3f outside [0.4,0.6]", id, ratio)
		}
	}
}

// ─── RecordResult + AnalyzeExperiment ─────────────

func TestCreateExperiment_AssignsIDAndDefaults(t *testing.T) {
	e := NewEngine(nil)
	exp := sampleExperiment()
	if err := e.CreateExperiment(context.Background(), exp); err != nil {
		t.Fatalf("create: %v", err)
	}
	if exp.ID == "" {
		t.Fatal("expected auto-generated ID")
	}
	if exp.Status != StatusDraft {
		t.Fatalf("expected draft status, got %s", exp.Status)
	}
}

func TestRecordResult_RoundTripsThroughEngine(t *testing.T) {
	e := NewEngine(nil)
	exp := sampleExperiment()
	_ = e.CreateExperiment(context.Background(), exp)
	r := ExperimentResult{
		ExperimentID: exp.ID,
		VariantID:    "a",
		UserID:       "u",
		Latency:      120 * time.Millisecond,
		QualityScore: 0.8,
		CostUSD:      0.001,
		Tokens:       250,
	}
	if err := e.RecordResult(context.Background(), r); err != nil {
		t.Fatalf("record: %v", err)
	}
	// One result in → analysis should see one sample on variant a.
	stats, err := e.collectStats(context.Background(), exp)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	got := 0
	for _, s := range stats {
		if s.VariantID == "a" {
			got = s.SampleCount
		}
	}
	if got != 1 {
		t.Fatalf("expected 1 sample for variant a, got %d", got)
	}
}

func TestAnalyze_LowSampleHasNoWinner(t *testing.T) {
	e := NewEngine(nil)
	exp := sampleExperiment()
	_ = e.CreateExperiment(context.Background(), exp)
	// 4 samples on each variant — below MinSamplesForAnalysis (10).
	for i := 0; i < 4; i++ {
		_ = e.RecordResult(context.Background(), ExperimentResult{
			ExperimentID: exp.ID, VariantID: "a", QualityScore: 0.9,
		})
		_ = e.RecordResult(context.Background(), ExperimentResult{
			ExperimentID: exp.ID, VariantID: "b", QualityScore: 0.5,
		})
	}
	a, err := e.AnalyzeExperiment(context.Background(), exp.ID)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if a.Winner != nil {
		t.Errorf("expected nil winner at low sample, got %v", *a.Winner)
	}
	if !strings.Contains(a.Recommendation, "Not enough") {
		t.Errorf("expected 'Not enough' recommendation, got %q", a.Recommendation)
	}
}

func TestAnalyze_WinnerDetectedAfterMinSamples(t *testing.T) {
	e := NewEngine(nil)
	exp := sampleExperiment()
	_ = e.CreateExperiment(context.Background(), exp)
	// 110 samples each — clears MinSamplesForWinner. Variant a
	// scores 0.9 vs b at 0.4 — clear winner on quality.
	for i := 0; i < 110; i++ {
		_ = e.RecordResult(context.Background(), ExperimentResult{
			ExperimentID: exp.ID, VariantID: "a", QualityScore: 0.9,
		})
		_ = e.RecordResult(context.Background(), ExperimentResult{
			ExperimentID: exp.ID, VariantID: "b", QualityScore: 0.4,
		})
	}
	a, err := e.AnalyzeExperiment(context.Background(), exp.ID)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if a.Winner == nil || *a.Winner != "a" {
		t.Fatalf("expected winner=a, got %v", a.Winner)
	}
	if a.Confidence <= 0 || a.Confidence > 1 {
		t.Errorf("confidence %.3f outside [0,1]", a.Confidence)
	}
}

func TestAnalyze_RespectsMetricChoice(t *testing.T) {
	// MetricCost wants the lowest avg cost. Variant b is cheaper.
	e := NewEngine(nil)
	exp := sampleExperiment()
	exp.Metric = MetricCost
	_ = e.CreateExperiment(context.Background(), exp)
	for i := 0; i < 110; i++ {
		_ = e.RecordResult(context.Background(), ExperimentResult{
			ExperimentID: exp.ID, VariantID: "a", CostUSD: 0.01,
		})
		_ = e.RecordResult(context.Background(), ExperimentResult{
			ExperimentID: exp.ID, VariantID: "b", CostUSD: 0.001,
		})
	}
	a, _ := e.AnalyzeExperiment(context.Background(), exp.ID)
	if a.Winner == nil || *a.Winner != "b" {
		t.Fatalf("cost winner should be b, got %v", a.Winner)
	}
}

// ─── auto-complete ────────────────────────────────

func TestCheckAutoComplete_StopsAfter1000Samples(t *testing.T) {
	e := NewEngine(nil)
	exp := sampleExperiment()
	_ = e.CreateExperiment(context.Background(), exp)
	_ = e.StartExperiment(context.Background(), exp.ID)

	for i := 0; i < AutoCompleteSamples+1; i++ {
		_ = e.RecordResult(context.Background(), ExperimentResult{
			ExperimentID: exp.ID, VariantID: "a", QualityScore: 0.5,
		})
	}
	e.CheckAutoComplete(context.Background())
	got, _ := e.GetExperiment(exp.ID)
	if got.Status != StatusCompleted {
		t.Fatalf("expected status=completed, got %s", got.Status)
	}
	if got.EndedAt == nil {
		t.Error("EndedAt should be set after auto-complete")
	}
}

func TestCheckAutoComplete_StopsAfterAge(t *testing.T) {
	e := NewEngine(nil)
	exp := sampleExperiment()
	_ = e.CreateExperiment(context.Background(), exp)
	_ = e.StartExperiment(context.Background(), exp.ID)
	// Backdate StartedAt past the auto-complete age.
	e.mu.Lock()
	old := time.Now().Add(-AutoCompleteAge - time.Hour)
	stored := e.experiments[exp.ID]
	stored.StartedAt = &old
	e.mu.Unlock()
	e.CheckAutoComplete(context.Background())
	got, _ := e.GetExperiment(exp.ID)
	if got.Status != StatusCompleted {
		t.Fatalf("expected status=completed after age cap, got %s", got.Status)
	}
}

// ─── active-experiment cap ────────────────────────

func TestStart_RespectsActiveCap(t *testing.T) {
	e := NewEngine(nil)
	// Create + start MaxActiveExperiments back-to-back.
	for i := 0; i < MaxActiveExperiments; i++ {
		exp := sampleExperiment()
		exp.Name = "exp-" + itoa(i)
		if err := e.CreateExperiment(context.Background(), exp); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		if err := e.StartExperiment(context.Background(), exp.ID); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	// One more should be rejected.
	exp := sampleExperiment()
	exp.Name = "over-cap"
	_ = e.CreateExperiment(context.Background(), exp)
	err := e.StartExperiment(context.Background(), exp.ID)
	if err == nil || !strings.Contains(err.Error(), "active experiments") {
		t.Fatalf("expected cap error, got %v", err)
	}
}

func TestStartStop_StatusTransitions(t *testing.T) {
	e := NewEngine(nil)
	exp := sampleExperiment()
	_ = e.CreateExperiment(context.Background(), exp)
	if got, _ := e.GetExperiment(exp.ID); got.Status != StatusDraft {
		t.Fatalf("status = %s, want draft", got.Status)
	}
	_ = e.StartExperiment(context.Background(), exp.ID)
	got, _ := e.GetExperiment(exp.ID)
	if got.Status != StatusRunning {
		t.Fatalf("status = %s, want running", got.Status)
	}
	if got.StartedAt == nil {
		t.Error("StartedAt should be set on first start")
	}
	_ = e.StopExperiment(context.Background(), exp.ID)
	got, _ = e.GetExperiment(exp.ID)
	if got.Status != StatusCompleted {
		t.Fatalf("status = %s, want completed", got.Status)
	}
	// Restarting a completed experiment must fail.
	if err := e.StartExperiment(context.Background(), exp.ID); err == nil {
		t.Error("expected error restarting completed experiment")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string('0'+rune(n%10)) + digits
		n /= 10
	}
	return digits
}

// ─── concurrent race-condition tests ──────────────

// TestStartExperiment_CompletedRejectedConcurrently covers the
// completed-status TOCTOU fix. Before the fix two goroutines could
// both pass the status check in separate critical sections; the
// entire check+mutate is now inside one Lock so every concurrent
// caller must receive an error.
func TestStartExperiment_CompletedRejectedConcurrently(t *testing.T) {
	e := NewEngine(nil)
	exp := sampleExperiment()
	_ = e.CreateExperiment(context.Background(), exp)
	_ = e.StartExperiment(context.Background(), exp.ID)
	_ = e.StopExperiment(context.Background(), exp.ID)

	const goroutines = 20
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			errs[i] = e.StartExperiment(context.Background(), exp.ID)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err == nil {
			t.Errorf("goroutine %d: expected error starting completed experiment, got nil", i)
		}
	}
	got, ok := e.GetExperiment(exp.ID)
	if !ok {
		t.Fatal("experiment not found after concurrent starts")
	}
	if got.Status != StatusCompleted {
		t.Fatalf("status = %s after concurrent starts, want completed", got.Status)
	}
}

// TestStartExperiment_ActiveCapHoldsUnderConcurrency covers the
// active-cap TOCTOU fix. Before the fix goroutines could slip past
// the cap check and mutate concurrently; cap enforcement is now
// inside the same write lock as the status mutation, so at most
// MaxActiveExperiments can ever end up StatusRunning.
func TestStartExperiment_ActiveCapHoldsUnderConcurrency(t *testing.T) {
	e := NewEngine(nil)
	const extra = 5
	total := MaxActiveExperiments + extra

	ids := make([]string, total)
	for i := 0; i < total; i++ {
		exp := sampleExperiment()
		exp.Name = "concurrent-" + itoa(i)
		_ = e.CreateExperiment(context.Background(), exp)
		ids[i] = exp.ID
	}

	var wg sync.WaitGroup
	wg.Add(total)
	for _, id := range ids {
		id := id
		go func() {
			defer wg.Done()
			_ = e.StartExperiment(context.Background(), id)
		}()
	}
	wg.Wait()

	running := 0
	for _, id := range ids {
		if exp, ok := e.GetExperiment(id); ok && exp.Status == StatusRunning {
			running++
		}
	}
	if running > MaxActiveExperiments {
		t.Fatalf("cap violated: %d experiments running, max is %d", running, MaxActiveExperiments)
	}
}
