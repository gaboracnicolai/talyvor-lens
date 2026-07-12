package keel

import (
	"math"
	"reflect"
	"testing"
)

// computeStats must mirror internal/anomaly.computeStats EXACTLY: population stddev = sqrt(sqDiff/n)
// (divide by n, NOT n-1). Hand-verified on a known slice so a drift from the temporal engine's statistic
// fails here. [1,2,3,4] → mean 2.5, var = ((1.5²+0.5²+0.5²+1.5²)/4)=1.25, stddev=sqrt(1.25).
func TestComputeStats_PopulationStdDevParity(t *testing.T) {
	s := computeStats([]float64{1, 2, 3, 4})
	if s == nil || s.n != 4 {
		t.Fatalf("computeStats nil/wrong n: %+v", s)
	}
	if math.Abs(s.mean-2.5) > 1e-12 {
		t.Errorf("mean = %v, want 2.5", s.mean)
	}
	if math.Abs(s.stdDev-math.Sqrt(1.25)) > 1e-12 {
		t.Errorf("stddev = %v, want sqrt(1.25)=%v (population /n, mirroring anomaly.computeStats)", s.stdDev, math.Sqrt(1.25))
	}
}

// cfg2 uses a 2σ cutoff for the synthetic scenarios (thresholds are placeholders; synthetic data only
// proves the MECHANISM).
func cfg2() Config { return Config{MinWorkspaces: 3, DeviationSigma: 2.0} }

// obs is a tiny builder: one (unit, ws, window, mean) point.
func obs(unit, ws string, win int64, q float64) Observation {
	return Observation{Unit: unit, WorkspaceID: ws, Window: win, MeanQuality: q, Sample: 10}
}

// IDIOSYNCRATIC: 6 opted-in workspaces on unit "openai/gpt-4o". Baseline: all ~0.8. Current: ONE (wsA)
// drifts to 0.2, the rest hold. wsA must be attributed IDIOSYNCRATIC; no one else flagged.
func TestDetect_IdiosyncraticAttributedToDrifter(t *testing.T) {
	var in []Observation
	wss := []string{"wsA", "wsB", "wsC", "wsD", "wsE", "wsF"}
	for _, ws := range wss {
		in = append(in, obs("openai/gpt-4o", ws, 1, 0.8)) // baseline window
	}
	// current window
	in = append(in, obs("openai/gpt-4o", "wsA", 2, 0.2)) // the drifter
	for _, ws := range wss[1:] {
		in = append(in, obs("openai/gpt-4o", ws, 2, 0.8))
	}

	fs := Detect(in, cfg2())

	idio := map[string]bool{}
	for _, f := range fs {
		if f.Attribution == AttributionIdiosyncratic {
			idio[f.WorkspaceID] = true
		}
	}
	if !idio["wsA"] {
		t.Errorf("wsA (the drifter) not attributed idiosyncratic; findings=%+v", fs)
	}
	if len(idio) != 1 {
		t.Errorf("exactly wsA should be idiosyncratic, got %v", idio)
	}
	// every finding carries the floor-satisfying cohort size and NO counterparty data (structural)
	for _, f := range fs {
		if f.CohortN < 3 {
			t.Errorf("finding below floor emitted: %+v", f)
		}
	}
}

// COMMON-MODE: a cohort with one PERSISTENT outlier, then ALL workspaces shift by the same constant
// (e.g. the model degraded). The outlier stays an outlier (|deviation|≥cutoff) but its RELATIVE position
// is unchanged → it must be labeled COMMON_MODE, and NOTHING may be labeled idiosyncratic.
func TestDetect_CommonModeNotIdiosyncratic(t *testing.T) {
	base := map[string]float64{"w1": 0.5, "w2": 0.5, "w3": 0.5, "w4": 0.5, "w5": 0.5, "w6": 0.9}
	var in []Observation
	for ws, q := range base {
		in = append(in, obs("anthropic/sonnet", ws, 1, q))
		in = append(in, obs("anthropic/sonnet", ws, 2, q-0.3)) // uniform −0.3 shift to ALL
	}

	fs := Detect(in, cfg2())

	sawCommonModeOutlier := false
	for _, f := range fs {
		if f.Attribution == AttributionIdiosyncratic {
			t.Errorf("common-mode shift wrongly attributed idiosyncratic: %+v", f)
		}
		if f.WorkspaceID == "w6" && f.Attribution == AttributionCommonMode {
			sawCommonModeOutlier = true
		}
	}
	if !sawCommonModeOutlier {
		t.Errorf("the persistent outlier w6 should surface as COMMON_MODE under a cohort-wide shift; findings=%+v", fs)
	}
}

// DETERMINISM: identical input → byte-identical output across runs (ordered aggregation/reduction). Float
// sigmas are real-valued (expected); the ORDER and VALUES must be reproducible.
func TestDetect_Deterministic(t *testing.T) {
	var in []Observation
	for _, ws := range []string{"z", "a", "m", "b", "q"} { // deliberately unsorted insertion
		in = append(in, obs("openai/gpt-4o", ws, 1, 0.7))
	}
	in = append(in, obs("openai/gpt-4o", "a", 2, 0.1))
	for _, ws := range []string{"z", "m", "b", "q"} {
		in = append(in, obs("openai/gpt-4o", ws, 2, 0.7))
	}
	first := Detect(in, cfg2())
	for i := 0; i < 20; i++ {
		if !reflect.DeepEqual(first, Detect(in, cfg2())) {
			t.Fatalf("non-deterministic output on run %d", i)
		}
	}
}

// FLOOR: a cohort of only 2 distinct workspaces is WITHHELD entirely — a lone other tenant's value is
// never recoverable (nothing emitted).
func TestDetect_BelowFloorWithholds(t *testing.T) {
	in := []Observation{
		obs("openai/gpt-4o", "wsA", 1, 0.8), obs("openai/gpt-4o", "wsB", 1, 0.8),
		obs("openai/gpt-4o", "wsA", 2, 0.2), obs("openai/gpt-4o", "wsB", 2, 0.8),
	}
	if fs := Detect(in, cfg2()); len(fs) != 0 {
		t.Errorf("below-floor cohort (2 workspaces) must emit nothing, got %+v", fs)
	}
}
