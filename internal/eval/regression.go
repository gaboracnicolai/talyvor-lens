package eval

// RegressionEpsilon is the minimum score drop that counts as a regression.
// Heuristic scores jitter a little run-to-run; ignoring drops smaller than this
// avoids crying wolf on noise while still catching real quality loss.
const RegressionEpsilon = 0.05

// Regression records one test case that scored worse than it did on the prior
// run of the same dataset.
type Regression struct {
	TestCaseID string  `json:"test_case_id"`
	TestName   string  `json:"test_name"`
	PrevScore  float64 `json:"prev_score"`
	CurrScore  float64 `json:"curr_score"`
	Delta      float64 `json:"delta"` // curr - prev; negative = worse
	PrevPassed bool    `json:"prev_passed"`
	CurrPassed bool    `json:"curr_passed"`
}

// detectRegressions compares a current run's per-case results against the prior
// run's, flagging any case whose score dropped by more than eps OR that flipped
// from passing to failing (crossing the pass threshold is meaningful even on a
// small score move). Cases without a prior-run baseline are skipped — a brand
// new case can't have regressed.
func detectRegressions(prev, curr []EvalResult, eps float64) []Regression {
	prevByID := make(map[string]EvalResult, len(prev))
	for _, r := range prev {
		prevByID[r.TestCaseID] = r
	}
	var out []Regression
	for _, c := range curr {
		p, ok := prevByID[c.TestCaseID]
		if !ok {
			continue // no baseline
		}
		worse := c.Score < p.Score-eps
		flipped := p.Passed && !c.Passed
		if worse || flipped {
			out = append(out, Regression{
				TestCaseID: c.TestCaseID,
				TestName:   c.TestName,
				PrevScore:  p.Score,
				CurrScore:  c.Score,
				Delta:      c.Score - p.Score,
				PrevPassed: p.Passed,
				CurrPassed: c.Passed,
			})
		}
	}
	return out
}
