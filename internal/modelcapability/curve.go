package modelcapability

// CurvePoint is one difficulty level of a model's capability curve: the mean
// quality observed at that difficulty and how many observations backed it.
type CurvePoint struct {
	Difficulty int     `json:"difficulty"`  // H1-derived work-tier ordinal, [0, MaxDifficulty]
	AvgQuality float64 `json:"avg_quality"` // mean composite quality [0,1] at this difficulty
	Samples    int     `json:"samples"`
}

// Curve is one model's fitted capability curve: the per-difficulty points plus a
// linear trend of quality vs difficulty. Slope < 0 means quality DEGRADES as
// work-tier rises; Slope ≈ 0 means the model HOLDS quality. Descriptive only —
// never a reward input.
type Curve struct {
	Model     string       `json:"model"`
	Provider  string       `json:"provider"`
	Points    []CurvePoint `json:"points"`
	Slope     float64      `json:"slope"`     // Δquality per difficulty step (fitted)
	Intercept float64      `json:"intercept"` // fitted quality at difficulty 0
	N         int          `json:"n"`         // total observations behind the curve
}

// fitLinear is ordinary least squares WEIGHTED by sample count — equivalent to
// OLS over the raw observations, since all observations at a difficulty share the
// same x. Fewer than two DISTINCT difficulty levels cannot define a line, so slope
// is 0 and intercept is the weighted mean (never NaN/Inf). Pure.
func fitLinear(pts []CurvePoint) (slope, intercept float64) {
	var sw, swx, swy, swxy, swxx float64
	for _, p := range pts {
		w := float64(p.Samples)
		x := float64(p.Difficulty)
		y := p.AvgQuality
		sw += w
		swx += w * x
		swy += w * y
		swxy += w * x * y
		swxx += w * x * x
	}
	if sw == 0 {
		return 0, 0
	}
	denom := sw*swxx - swx*swx
	if denom == 0 { // <2 distinct difficulty levels — no line, just the mean
		return 0, swy / sw
	}
	slope = (sw*swxy - swx*swy) / denom
	intercept = (swy - slope*swx) / sw
	return slope, intercept
}
