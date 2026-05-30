// significance.go EXTENDS the Engine with proper statistical-significance
// testing on quality. It reuses the Engine's existing machinery — deterministic
// AssignVariant bucketing (engine.go) and the per-variant QualityScore samples
// already recorded via RecordResult/RecordResultAsync — and layers a real
// hypothesis test on top via the dependency-free internal/stats package.
//
// The honesty contract is the whole point: a winner is NEVER declared from a
// tiny sample or a non-significant difference. "Inconclusive" is a first-class,
// prominent outcome (see internal/stats.Compare).
package ab

import (
	"context"
	"fmt"

	"github.com/talyvor/lens/internal/metrics"
	"github.com/talyvor/lens/internal/stats"
)

// VariantRef identifies a variant in a significance comparison with its sample
// size and mean quality (so the dashboard can show "B looks better" alongside
// the honest verdict that the difference isn't significant).
type VariantRef struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	N           int     `json:"n"`
	MeanQuality float64 `json:"mean_quality"`
}

// SignificanceReport is the GET /eval/ab/:experiment payload: the two variants
// compared (highest-mean vs runner-up), the full statistical verdict, and a
// top-level Significant flag + plain-language Summary that surfaces
// "inconclusive" prominently when the data doesn't support a winner.
type SignificanceReport struct {
	ExperimentID string        `json:"experiment_id"`
	Metric       string        `json:"metric"` // always "quality" here
	VariantA     VariantRef    `json:"variant_a"`
	VariantB     VariantRef    `json:"variant_b"`
	Verdict      stats.Verdict `json:"verdict"`
	Significant  bool          `json:"significant"`
	Summary      string        `json:"summary"`
}

const qualitySamplesSQL = `
SELECT variant_id, quality_score
FROM experiment_results
WHERE experiment_id = $1`

// collectQualitySamples returns the raw per-variant quality scores. In-memory
// mode (nil pool) walks the buffer; DB mode reads experiment_results. This is
// the raw-sample counterpart to collectStats (which only returns averages).
func (e *Engine) collectQualitySamples(ctx context.Context, exp *Experiment) (map[string][]float64, error) {
	out := map[string][]float64{}
	if e.pool == nil {
		e.mu.RLock()
		for _, r := range e.resultsBuf {
			if r.ExperimentID == exp.ID {
				out[r.VariantID] = append(out[r.VariantID], r.QualityScore)
			}
		}
		e.mu.RUnlock()
		return out, nil
	}
	rows, err := e.pool.Query(ctx, qualitySamplesSQL, exp.ID)
	if err != nil {
		return nil, fmt.Errorf("ab: quality samples query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var vid string
		var score float64
		if err := rows.Scan(&vid, &score); err != nil {
			return nil, fmt.Errorf("ab: scan quality sample: %w", err)
		}
		out[vid] = append(out[vid], score)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Significance computes the statistical-significance verdict for an experiment's
// quality metric: it compares the highest-mean-quality variant against the
// runner-up using the Mann-Whitney U test (primary) with Welch's t as a
// secondary. It NEVER declares significance from fewer than two variants with
// data, nor from samples below internal/stats.MinSamplesPerGroup.
func (e *Engine) Significance(ctx context.Context, experimentID string) (*SignificanceReport, error) {
	exp, ok := e.GetExperiment(experimentID)
	if !ok {
		return nil, fmt.Errorf("ab: experiment %q not found", experimentID)
	}
	samples, err := e.collectQualitySamples(ctx, exp)
	if err != nil {
		return nil, err
	}

	// Build per-variant refs (catalogue order), keep only those with data.
	var cands []sigCandidate
	for _, v := range exp.Variants {
		s := samples[v.ID]
		if len(s) == 0 {
			continue
		}
		cands = append(cands, sigCandidate{
			ref:     VariantRef{ID: v.ID, Name: v.Name, N: len(s), MeanQuality: meanOf(s)},
			samples: s,
		})
	}

	rep := &SignificanceReport{ExperimentID: experimentID, Metric: "quality"}

	if len(cands) < 2 {
		rep.Significant = false
		rep.Summary = "inconclusive — need at least two variants with recorded results to compare"
		if len(cands) == 1 {
			rep.VariantA = cands[0].ref
		}
		return rep, nil
	}

	// Compare the two highest-mean variants: "best" vs "runner-up".
	best, runner := topTwoByMean(cands)
	a, b := cands[best], cands[runner]

	v := stats.Compare(a.samples, b.samples, 0.05)
	rep.VariantA = a.ref
	rep.VariantB = b.ref
	rep.Verdict = v
	rep.Significant = v.Significant
	rep.Summary = v.Summary
	if rep.Significant {
		metrics.ABSignificantResult()
	}
	return rep, nil
}

func meanOf(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// sigCandidate pairs a variant's display ref with its raw quality samples.
type sigCandidate struct {
	ref     VariantRef
	samples []float64
}

// topTwoByMean returns the indices of the highest and second-highest
// mean-quality candidates.
func topTwoByMean(cands []sigCandidate) (int, int) {
	best, runner := 0, 1
	if cands[runner].ref.MeanQuality > cands[best].ref.MeanQuality {
		best, runner = runner, best
	}
	for i := 2; i < len(cands); i++ {
		m := cands[i].ref.MeanQuality
		if m > cands[best].ref.MeanQuality {
			runner = best
			best = i
		} else if m > cands[runner].ref.MeanQuality {
			runner = i
		}
	}
	return best, runner
}
