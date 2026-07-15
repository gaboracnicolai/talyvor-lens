package routingbrain

import (
	"math"
	"testing"
)

// TestQualityAt_ClampsAndFollowsSlope — the fitted capability value at a difficulty
// is intercept+slope*d, clamped to [0,1]. A degrading curve yields lower quality as
// difficulty rises.
func TestQualityAt_ClampsAndFollowsSlope(t *testing.T) {
	c := ModelCurve{Model: "m", Intercept: 0.9, Slope: -0.10}
	if got := c.QualityAt(0); math.Abs(got-0.9) > 1e-9 {
		t.Errorf("QualityAt(0)=%v want 0.9", got)
	}
	if got := c.QualityAt(6); math.Abs(got-0.3) > 1e-9 {
		t.Errorf("QualityAt(6)=%v want 0.3", got)
	}
	// clamp
	if got := (ModelCurve{Intercept: 2.0}).QualityAt(0); got != 1.0 {
		t.Errorf("QualityAt clamp-high=%v want 1.0", got)
	}
	if got := (ModelCurve{Intercept: -1.0}).QualityAt(0); got != 0.0 {
		t.Errorf("QualityAt clamp-low=%v want 0.0", got)
	}
}

// TestCompute_PicksHighestVerifiedQuality_CurveDriven — the headline learning proof.
// The pick is driven by the CAPABILITY CURVE at each difficulty, not a static rank:
// model A (high at low difficulty, degrades) wins at d=0, but the flat model B wins
// at d=6 where A has degraded below it. The Keel-ADVERSE model C is dropped at every
// difficulty despite its top intercept.
func TestCompute_PicksHighestVerifiedQuality_CurveDriven(t *testing.T) {
	in := ComputeInputs{
		MaxDifficulty: 6,
		Workspaces:    []WorkspaceModels{{WorkspaceID: "ws1", AllowedModels: []string{"A", "B", "C"}}},
		Curves: []ModelCurve{
			{Model: "A", Provider: "openai", Intercept: 0.90, Slope: -0.10}, // degrades
			{Model: "B", Provider: "openai", Intercept: 0.60, Slope: 0.0},   // holds
			{Model: "C", Provider: "openai", Intercept: 0.95, Slope: -0.20}, // best-looking but ADVERSE
		},
		Adverse: map[string]map[string]bool{"ws1": {"C": true}},
		Cost:    func(string) float64 { return 1.0 },
	}
	recs := Compute(in)
	byDiff := map[int]Recommendation{}
	for _, r := range recs {
		if r.WorkspaceID != "ws1" {
			t.Fatalf("unexpected workspace %q", r.WorkspaceID)
		}
		if !r.Verified {
			t.Errorf("computed rec must be Verified (passed allow-list ∩ curve ∩ not-adverse): %+v", r)
		}
		if r.Model == "C" {
			t.Errorf("Keel-ADVERSE model C must never be recommended: %+v", r)
		}
		byDiff[r.Difficulty] = r
	}
	if len(recs) != in.MaxDifficulty+1 {
		t.Fatalf("expected one rec per difficulty 0..%d; got %d", in.MaxDifficulty, len(recs))
	}
	if byDiff[0].Model != "A" {
		t.Errorf("d=0 best verified is A (0.90); got %q", byDiff[0].Model)
	}
	if byDiff[6].Model != "B" {
		t.Errorf("d=6 best verified is B (0.60 > A's degraded 0.30); got %q", byDiff[6].Model)
	}
	if byDiff[0].Provider != "openai" {
		t.Errorf("provider must come from the curve; got %q", byDiff[0].Provider)
	}
}

// TestCompute_DropsUnallowedAndCurveless — a candidate must be BOTH allow-listed AND
// have a capability curve. A workspace whose allow-list has no curved model yields no
// recommendations.
func TestCompute_DropsUnallowedAndCurveless(t *testing.T) {
	in := ComputeInputs{
		MaxDifficulty: 2,
		Workspaces: []WorkspaceModels{
			{WorkspaceID: "ws1", AllowedModels: []string{"A", "NOCURVE"}}, // NOCURVE dropped
			{WorkspaceID: "ws2", AllowedModels: []string{"NOTALLOWEDCURVE"}},
		},
		Curves: []ModelCurve{
			{Model: "A", Provider: "p", Intercept: 0.5},
			{Model: "OTHER", Provider: "p", Intercept: 0.99}, // ws2 not allowed to use it
		},
		Cost: func(string) float64 { return 1.0 },
	}
	recs := Compute(in)
	for _, r := range recs {
		if r.WorkspaceID == "ws2" {
			t.Errorf("ws2 has no allowed+curved model — must get no recommendation: %+v", r)
		}
		if r.Model != "A" {
			t.Errorf("only A is allowed AND curved for ws1; got %q", r.Model)
		}
	}
	if len(recs) != in.MaxDifficulty+1 { // ws1 × 3 difficulties
		t.Errorf("expected %d recs (ws1 only); got %d", in.MaxDifficulty+1, len(recs))
	}
}

// TestCompute_TieBreakLowerCostThenName — equal quality breaks to lower cost, then
// deterministically to model name.
func TestCompute_TieBreakLowerCostThenName(t *testing.T) {
	in := ComputeInputs{
		MaxDifficulty: 0,
		Workspaces:    []WorkspaceModels{{WorkspaceID: "ws1", AllowedModels: []string{"cheap", "pricey", "cheap2"}}},
		Curves: []ModelCurve{
			{Model: "cheap", Provider: "p", Intercept: 0.8},
			{Model: "pricey", Provider: "p", Intercept: 0.8},
			{Model: "cheap2", Provider: "p", Intercept: 0.8},
		},
		Cost: func(m string) float64 {
			if m == "pricey" {
				return 5.0
			}
			return 1.0 // cheap and cheap2 tie on cost → name tiebreak → "cheap"
		},
	}
	recs := Compute(in)
	if len(recs) != 1 || recs[0].Model != "cheap" {
		t.Errorf("tie must break to lower cost then name 'cheap'; got %+v", recs)
	}
}

// TestCompute_Deterministic — same inputs → byte-identical output ordering.
func TestCompute_Deterministic(t *testing.T) {
	in := ComputeInputs{
		MaxDifficulty: 3,
		Workspaces:    []WorkspaceModels{{WorkspaceID: "wsB", AllowedModels: []string{"A"}}, {WorkspaceID: "wsA", AllowedModels: []string{"A"}}},
		Curves:        []ModelCurve{{Model: "A", Provider: "p", Intercept: 0.7}},
		Cost:          func(string) float64 { return 1.0 },
	}
	first := Compute(in)
	for i := 0; i < 5; i++ {
		again := Compute(in)
		if len(again) != len(first) {
			t.Fatalf("nondeterministic length")
		}
		for j := range first {
			if again[j] != first[j] {
				t.Fatalf("nondeterministic order at %d: %+v vs %+v", j, again[j], first[j])
			}
		}
	}
}
