package modelcapability

import (
	"math"
	"testing"
)

const eps = 1e-9

// TestFitLinear_PerfectNegativeTrend — a model whose quality falls a fixed amount
// per difficulty step fits an exact line: slope = −0.10, intercept = 0.95. This is
// the "degrader" the acceptance test relies on.
func TestFitLinear_PerfectNegativeTrend(t *testing.T) {
	var pts []CurvePoint
	for d := 0; d <= MaxDifficulty; d++ {
		pts = append(pts, CurvePoint{Difficulty: d, AvgQuality: 0.95 - 0.10*float64(d), Samples: 5})
	}
	slope, intercept := fitLinear(pts)
	if math.Abs(slope-(-0.10)) > eps {
		t.Errorf("slope = %v, want -0.10", slope)
	}
	if math.Abs(intercept-0.95) > eps {
		t.Errorf("intercept = %v, want 0.95", intercept)
	}
}

// TestFitLinear_FlatTrend — a model that HOLDS quality regardless of difficulty
// fits slope ≈ 0. This is the "holder".
func TestFitLinear_FlatTrend(t *testing.T) {
	var pts []CurvePoint
	for d := 0; d <= MaxDifficulty; d++ {
		pts = append(pts, CurvePoint{Difficulty: d, AvgQuality: 0.80, Samples: 5})
	}
	slope, intercept := fitLinear(pts)
	if math.Abs(slope) > eps {
		t.Errorf("slope = %v, want ~0", slope)
	}
	if math.Abs(intercept-0.80) > eps {
		t.Errorf("intercept = %v, want 0.80", intercept)
	}
}

// TestFitLinear_Degenerate — fewer than two DISTINCT difficulty levels cannot fit
// a line: slope is 0 and intercept is the (weighted) mean quality. Never NaN/Inf.
func TestFitLinear_Degenerate(t *testing.T) {
	for _, pts := range [][]CurvePoint{
		nil,
		{{Difficulty: 2, AvgQuality: 0.7, Samples: 9}},
		{{Difficulty: 3, AvgQuality: 0.6, Samples: 2}, {Difficulty: 3, AvgQuality: 0.8, Samples: 2}}, // same x
	} {
		slope, intercept := fitLinear(pts)
		if math.IsNaN(slope) || math.IsInf(slope, 0) || math.IsNaN(intercept) || math.IsInf(intercept, 0) {
			t.Errorf("degenerate fit produced non-finite slope=%v intercept=%v for %+v", slope, intercept, pts)
		}
		if slope != 0 {
			t.Errorf("degenerate slope = %v, want 0 for %+v", slope, pts)
		}
	}
}

// TestFitLinear_SampleWeighted — the fit is WEIGHTED by sample count, so a heavily
// observed difficulty level pulls the line. Hand-computed reference: points
// (0,0,w1),(1,0,w1),(2,3,w2) with w=1,1,2 → slope=18/11, intercept=-6/11.
func TestFitLinear_SampleWeighted(t *testing.T) {
	pts := []CurvePoint{
		{Difficulty: 0, AvgQuality: 0.0, Samples: 1},
		{Difficulty: 1, AvgQuality: 0.0, Samples: 1},
		{Difficulty: 2, AvgQuality: 3.0, Samples: 2},
	}
	slope, intercept := fitLinear(pts)
	if math.Abs(slope-18.0/11.0) > eps {
		t.Errorf("weighted slope = %v, want %v", slope, 18.0/11.0)
	}
	if math.Abs(intercept-(-6.0/11.0)) > eps {
		t.Errorf("weighted intercept = %v, want %v", intercept, -6.0/11.0)
	}
}
