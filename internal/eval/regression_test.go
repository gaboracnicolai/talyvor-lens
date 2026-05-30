package eval

import "testing"

func TestDetectRegressions_FlagsWorseCase(t *testing.T) {
	prev := []EvalResult{
		{TestCaseID: "a", TestName: "A", Score: 0.90, Passed: true},
		{TestCaseID: "b", TestName: "B", Score: 0.80, Passed: true},
	}
	curr := []EvalResult{
		{TestCaseID: "a", TestName: "A", Score: 0.50, Passed: false}, // big drop
		{TestCaseID: "b", TestName: "B", Score: 0.81, Passed: true},  // stable
	}
	regs := detectRegressions(prev, curr, RegressionEpsilon)
	if len(regs) != 1 {
		t.Fatalf("want 1 regression, got %d: %+v", len(regs), regs)
	}
	if regs[0].TestCaseID != "a" {
		t.Errorf("flagged %q, want a", regs[0].TestCaseID)
	}
	if regs[0].Delta >= 0 {
		t.Errorf("delta = %v, want negative", regs[0].Delta)
	}
}

func TestDetectRegressions_NoFalseFlagWhenStable(t *testing.T) {
	prev := []EvalResult{
		{TestCaseID: "a", Score: 0.90, Passed: true},
		{TestCaseID: "b", Score: 0.80, Passed: true},
	}
	curr := []EvalResult{
		{TestCaseID: "a", Score: 0.92, Passed: true}, // improved
		{TestCaseID: "b", Score: 0.78, Passed: true}, // drop < epsilon
	}
	regs := detectRegressions(prev, curr, RegressionEpsilon)
	if len(regs) != 0 {
		t.Fatalf("stable run should flag nothing, got %+v", regs)
	}
}

func TestDetectRegressions_FlagsPassToFailFlip(t *testing.T) {
	// Small score drop but it crossed the pass threshold — that's a regression.
	prev := []EvalResult{{TestCaseID: "a", Score: 0.81, Passed: true}}
	curr := []EvalResult{{TestCaseID: "a", Score: 0.79, Passed: false}}
	regs := detectRegressions(prev, curr, RegressionEpsilon)
	if len(regs) != 1 {
		t.Fatalf("pass→fail flip should be flagged, got %+v", regs)
	}
	if regs[0].PrevPassed != true || regs[0].CurrPassed != false {
		t.Errorf("flip flags wrong: %+v", regs[0])
	}
}

func TestDetectRegressions_SkipsCasesAbsentFromPrior(t *testing.T) {
	prev := []EvalResult{{TestCaseID: "a", Score: 0.9, Passed: true}}
	curr := []EvalResult{
		{TestCaseID: "a", Score: 0.9, Passed: true},
		{TestCaseID: "new", Score: 0.1, Passed: false}, // no baseline
	}
	regs := detectRegressions(prev, curr, RegressionEpsilon)
	if len(regs) != 0 {
		t.Fatalf("new case has no baseline; should not be a regression, got %+v", regs)
	}
}
