package routingbrain

import (
	"fmt"
	"math"
	"sort"
)

// WorkspaceModels is a workspace + its model allow-list — the per-workspace input
// to the offline learn step (read from the workspace manager).
type WorkspaceModels struct {
	WorkspaceID   string
	AllowedModels []string
}

// ModelCurve is one model's H2 capability curve reduced to what the brain needs:
// the fitted quality-at-difficulty line. Built from modelcapability.Curve
// (Intercept + Slope·difficulty).
type ModelCurve struct {
	Model     string
	Provider  string
	Intercept float64
	Slope     float64
}

// QualityAt is the fitted quality at a difficulty, clamped to the [0,1] quality
// scale. A degrading curve (Slope<0) yields lower quality as work-tier rises.
func (c ModelCurve) QualityAt(difficulty int) float64 {
	q := c.Intercept + c.Slope*float64(difficulty)
	if q < 0 {
		return 0
	}
	if q > 1 {
		return 1
	}
	return q
}

// ComputeInputs is everything the offline learn step needs, ALREADY read from the
// DB — so Compute is a pure, deterministic function of verified outcomes. Cost is a
// blended per-model price (lower = cheaper). Adverse[ws] is the set of models Keel
// flagged as adverse-drifting for that workspace (dropped from candidacy).
type ComputeInputs struct {
	MaxDifficulty int
	Workspaces    []WorkspaceModels
	Curves        []ModelCurve
	Adverse       map[string]map[string]bool
	Cost          func(model string) float64
}

// candidate is one model in the running for a (workspace, difficulty) cohort.
type candidate struct {
	model, provider string
	quality, cost   float64
}

const qualityEpsilon = 1e-12

// better reports whether a is a STRICTLY better pick than b: higher quality, then
// lower cost, then lexicographically smaller model name (a total, deterministic order).
func better(a, b candidate) bool {
	if math.Abs(a.quality-b.quality) > qualityEpsilon {
		return a.quality > b.quality
	}
	if a.cost != b.cost {
		return a.cost < b.cost
	}
	return a.model < b.model
}

// Compute is the offline brain: for every (workspace, difficulty 0..MaxDifficulty)
// it forms the candidate set (models the workspace is ALLOWED to use that HAVE a
// capability curve and are NOT Keel-adverse for the workspace), picks the best by
// capability-at-that-difficulty (tie: cheaper, then name), and emits one
// Recommendation. A cohort with no candidate emits none. Pure/deterministic — no
// DB, no clock, no money. Verified=true records that the pick passed the
// allow-list ∩ curve ∩ not-adverse filter at compute time.
func Compute(in ComputeInputs) []Recommendation {
	cost := in.Cost
	if cost == nil {
		cost = func(string) float64 { return 0 }
	}
	curveByModel := make(map[string]ModelCurve, len(in.Curves))
	for _, c := range in.Curves {
		curveByModel[c.Model] = c
	}
	ws := append([]WorkspaceModels(nil), in.Workspaces...)
	sort.Slice(ws, func(i, j int) bool { return ws[i].WorkspaceID < ws[j].WorkspaceID })

	var out []Recommendation
	for _, w := range ws {
		adverse := in.Adverse[w.WorkspaceID]
		for d := 0; d <= in.MaxDifficulty; d++ {
			var best candidate
			found := false
			for _, m := range w.AllowedModels {
				c, ok := curveByModel[m]
				if !ok {
					continue // no capability curve → not a candidate
				}
				if adverse[m] {
					continue // Keel-adverse for this workspace → never recommend
				}
				cand := candidate{model: c.Model, provider: c.Provider, quality: c.QualityAt(d), cost: cost(m)}
				if !found || better(cand, best) {
					best, found = cand, true
				}
			}
			if !found {
				continue
			}
			out = append(out, Recommendation{
				WorkspaceID: w.WorkspaceID, Difficulty: d,
				Model: best.model, Provider: best.provider,
				ExpectedQuality: best.quality, Verified: true,
				Reason: fmt.Sprintf("best verified quality %.3f at tier %d", best.quality, d),
			})
		}
	}
	return out
}
