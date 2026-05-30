package eval

import (
	"errors"
	"math"
	"testing"
)

func TestEstimateCost_UsesTargetModelAndCatalogPricing(t *testing.T) {
	// 400-char prompt → ~100 input tokens; default 256 output tokens.
	// gpt-4o-mini = $0.15/1M in, $0.60/1M out (from the catalog).
	prompt := make([]byte, 400)
	for i := range prompt {
		prompt[i] = 'x'
	}
	cases := []TestCase{
		{ID: "1", Model: "gpt-4o", Provider: "openai", Prompt: string(prompt)},
	}
	// Target overrides the model to gpt-4o-mini.
	est := estimateCost(cases, Target{Model: "gpt-4o-mini", Provider: "openai"})

	if est.Cases != 1 {
		t.Fatalf("cases = %d, want 1", est.Cases)
	}
	want := (100*0.15 + 256*0.60) / 1_000_000.0
	if math.Abs(est.EstCostUSD-want) > 1e-9 {
		t.Errorf("est = %.10f, want %.10f", est.EstCostUSD, want)
	}
}

func TestEstimateCost_FallsBackToCaseModel(t *testing.T) {
	cases := []TestCase{
		{ID: "1", Model: "gpt-4o-mini", Provider: "openai", Prompt: "short"},
		{ID: "2", Model: "gpt-4o-mini", Provider: "openai", Prompt: "short"},
	}
	est := estimateCost(cases, Target{}) // no override
	if est.Cases != 2 || est.EstCostUSD <= 0 {
		t.Fatalf("est = %+v, want 2 cases and positive cost", est)
	}
}

func TestCheckCostCap_RejectsOverBudget(t *testing.T) {
	est := CostEstimate{Cases: 1000, EstCostUSD: 12.50}
	if err := checkCostCap(est, 5.00); err == nil {
		t.Fatal("estimate over cap must error before running — no silent burn")
	} else if !errors.Is(err, ErrCostCapExceeded) {
		t.Errorf("want ErrCostCapExceeded, got %v", err)
	}
	// Within cap → no error.
	if err := checkCostCap(est, 20.00); err != nil {
		t.Errorf("within cap should pass, got %v", err)
	}
	// Cap of 0 = unlimited (no enforcement).
	if err := checkCostCap(est, 0); err != nil {
		t.Errorf("cap 0 means unlimited, got %v", err)
	}
}
