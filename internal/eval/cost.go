package eval

import (
	"errors"
	"fmt"

	"github.com/talyvor/lens/internal/alerts"
)

// DefaultEstOutputTokens is the assumed per-case response length used when
// estimating a run's cost BEFORE execution — we can't know the real response
// length until the model answers, so we budget a fixed, documented amount.
const DefaultEstOutputTokens = 256

// ErrCostCapExceeded is returned by checkCostCap when a run's estimated cost
// exceeds the caller-supplied cap. RunEval refuses to start in that case so a
// large suite can never silently burn unbounded spend.
var ErrCostCapExceeded = errors.New("eval: estimated cost exceeds cap")

// Target overrides what a dataset run is evaluated against, so the same inputs
// can be scored on a different model to answer "does switching to model X
// regress quality?". Empty fields fall back to each case's own provider/model.
// MaxCostUSD caps the run's estimated spend (0 = unlimited).
type Target struct {
	Model      string  `json:"model,omitempty"`
	Provider   string  `json:"provider,omitempty"`
	MaxCostUSD float64 `json:"max_cost_usd,omitempty"`
}

func (t Target) modelFor(tc TestCase) string {
	if t.Model != "" {
		return t.Model
	}
	return tc.Model
}

// CostEstimate is the projected spend for a dataset run, surfaced before
// execution (and returned with the run) so eval cost is always visible.
type CostEstimate struct {
	Cases           int     `json:"cases"`
	EstInputTokens  int     `json:"est_input_tokens"`
	EstOutputTokens int     `json:"est_output_tokens"`
	EstCostUSD      float64 `json:"est_cost_usd"`
}

// estimateCost projects the cost of running cases against the (optionally
// overridden) target model. Input tokens are estimated from prompt length
// (len/4, matching the live eval path); output tokens use the fixed
// DefaultEstOutputTokens. Pricing goes through alerts.CostUSD so the estimate
// uses the exact same catalog rates as real spend.
func estimateCost(cases []TestCase, target Target) CostEstimate {
	est := CostEstimate{Cases: len(cases)}
	for _, tc := range cases {
		in := len(tc.Prompt) / 4
		out := DefaultEstOutputTokens
		est.EstInputTokens += in
		est.EstOutputTokens += out
		est.EstCostUSD += alerts.CostUSD(target.modelFor(tc), in, out)
	}
	return est
}

// checkCostCap returns ErrCostCapExceeded when est.EstCostUSD exceeds cap. A
// cap of 0 (or negative) means unlimited — no enforcement.
func checkCostCap(est CostEstimate, cap float64) error {
	if cap <= 0 {
		return nil
	}
	if est.EstCostUSD > cap {
		return fmt.Errorf("%w: estimated $%.4f > cap $%.2f (%d cases)",
			ErrCostCapExceeded, est.EstCostUSD, cap, est.Cases)
	}
	return nil
}
