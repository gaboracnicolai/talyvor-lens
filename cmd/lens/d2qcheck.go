package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/discriminator"
	"github.com/talyvor/lens/internal/doc2query"
	"github.com/talyvor/lens/internal/embedder"
	"github.com/talyvor/lens/internal/poolsafety"
)

// `lens d2qcheck` — does deriving questions from stored ANSWERS widen recall, and at what cost to
// the danger ceiling?
//
// ⚠ IT SIMULATES THE REAL PIPELINE, not a proxy for it. For each pair (A,B): A is asked, a real
// answer is GENERATED for it, variants are derived FROM THAT ANSWER, and then B is scored against
// {A} ∪ variants — which is exactly the vector set the pooled read would search. Scoring B against
// the corpus questions directly would measure something doc2query does not do.
//
// ⚠ THE GATE IS APPLIED THROUGHOUT, on the original's discriminators, because that is what variant
// rows inherit. A pair the gate refuses cannot pool through ANY variant, so doc2query cannot widen
// the danger surface across an entity boundary — only inside one.
type d2qResult struct {
	Pair       poolsafety.RephrasePair
	Baseline   float64 // B vs A — what the pool could do before doc2query
	Best       float64 // B vs the best of {A} ∪ variants
	BestVia    string
	GateAllows bool
	NumVars    int
}

func runD2QCheck() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("d2qcheck: config: %w", err)
	}
	if cfg.OpenAIAPIKey == "" || cfg.AnthropicAPIKey == "" {
		return errors.New("d2qcheck: needs LENS_OPENAI_API_KEY (embeddings) and LENS_ANTHROPIC_API_KEY (deriving)")
	}
	emb := embedder.NewOpenAIEmbedder(cfg.OpenAIAPIKey, cfg.EmbeddingModel, cfg.EmbeddingBaseURL)
	der := doc2query.NewAnthropicDeriver(cfg.AnthropicAPIKey, "claude-haiku-4-5")
	gen := doc2query.NewAnthropicDeriver(cfg.AnthropicAPIKey, "claude-haiku-4-5")
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Minute)
	defer cancel()

	maxVars := 8
	corpora := []struct {
		name  string
		pairs []poolsafety.RephrasePair
	}{
		{"REPHRASINGS (recall)", poolsafety.EngineeringRephrasePairs()},
		{"DANGER design (precision)", poolsafety.EngineeringDangerPairs()},
		{"DANGER held-out (precision)", poolsafety.HeldOutDangerPairs()},
	}

	all := map[string][]d2qResult{}
	for _, c := range corpora {
		res, err := measureCorpus(ctx, emb, der, gen, c.pairs, maxVars)
		if err != nil {
			return fmt.Errorf("d2qcheck %s: %w", c.name, err)
		}
		all[c.name] = res
	}

	fmt.Printf("doc2query measurement — embedder %s, deriver claude-haiku-4-5, up to %d variants\n", cfg.EmbeddingModel, maxVars)
	for _, c := range corpora {
		res := all[c.name]
		fmt.Printf("\n=== %s (%d pairs) ===\n", c.name, len(res))
		sort.Slice(res, func(i, j int) bool { return res[i].Best > res[j].Best })
		for _, r := range res {
			gate := "gate:REFUSED"
			if r.GateAllows {
				gate = "gate:allowed"
			}
			fmt.Printf("  base %.4f -> best %.4f (%+.4f) %-13s vars=%d %-20s via %q\n",
				r.Baseline, r.Best, r.Best-r.Baseline, gate, r.NumVars, r.Pair.Name, r.BestVia)
		}
	}

	// ⚠ THE VARIANT-COUNT SWEEP. More vectors buy recall and cost precision; the point where the
	// second stops being worth the first is a measurement, not a guess.
	fmt.Printf("\n=== VARIANT-COUNT SWEEP ===\n")
	fmt.Printf("  N   rephrasings served    danger admitted (gate-allowed only)\n")
	for _, n := range []int{0, 1, 3, 5, 8} {
		for _, th := range []float64{0.92, 0.85, 0.83} {
			served := countAt(all["REPHRASINGS (recall)"], n, th, true)
			d1 := countAt(all["DANGER design (precision)"], n, th, true)
			d2 := countAt(all["DANGER held-out (precision)"], n, th, true)
			fmt.Printf("  %d   @%.2f  %2d/28              %d\n", n, th, served, d1+d2)
		}
	}
	return nil
}

func countAt(rs []d2qResult, n int, th float64, gated bool) int {
	var c int
	for _, r := range rs {
		if gated && !r.GateAllows {
			continue
		}
		s := r.Baseline
		if n > 0 && r.NumVars > 0 && r.Best > s {
			// Best is over all variants; approximate the first n by capping.
			if n >= r.NumVars {
				s = r.Best
			} else {
				s = r.BestAtN(n)
			}
		}
		if s >= th {
			c++
		}
	}
	return c
}

// varScores retains per-variant scores so the sweep is a real measurement rather than an
// interpolation between "none" and "all".
var varScores sync.Map // pairName -> []float64

func (r d2qResult) BestAtN(n int) float64 {
	v, ok := varScores.Load(r.Pair.Name)
	if !ok {
		return r.Baseline
	}
	ss := v.([]float64)
	best := r.Baseline
	for i, s := range ss {
		if i >= n {
			break
		}
		if s > best {
			best = s
		}
	}
	return best
}

func measureCorpus(ctx context.Context, emb poolsafety.Embedder, der, gen doc2query.Deriver, pairs []poolsafety.RephrasePair, maxVars int) ([]d2qResult, error) {
	out := make([]d2qResult, 0, len(pairs))
	for _, p := range pairs {
		vb, err := emb.Embed(ctx, p.B)
		if err != nil {
			return nil, err
		}
		va, err := emb.Embed(ctx, p.A)
		if err != nil {
			return nil, err
		}
		base, err := poolsafety.CosineOf(va, vb)
		if err != nil {
			return nil, err
		}

		// A real answer to A, then variants derived from THAT answer.
		answer, err := generateAnswer(ctx, gen, p.A)
		if err != nil {
			return nil, err
		}
		qs, err := der.Derive(ctx, answer, maxVars)
		if err != nil {
			return nil, err
		}
		best, via := base, p.A
		scores := make([]float64, 0, len(qs))
		for _, q := range qs {
			vq, err := emb.Embed(ctx, q)
			if err != nil {
				return nil, err
			}
			s, err := poolsafety.CosineOf(vq, vb)
			if err != nil {
				return nil, err
			}
			scores = append(scores, s)
			if s > best {
				best, via = s, q
			}
		}
		varScores.Store(p.Name, scores)
		out = append(out, d2qResult{
			Pair: p, Baseline: base, Best: best, BestVia: via,
			GateAllows: discriminator.Match(p.A, p.B), NumVars: len(qs),
		})
	}
	return out, nil
}

// generateAnswer produces the stored answer the pool would actually hold. Reuses the deriver's
// transport with a prompt that asks for an answer rather than questions.
func generateAnswer(ctx context.Context, gen doc2query.Deriver, question string) (string, error) {
	ad, ok := gen.(*doc2query.AnthropicDeriver)
	if !ok {
		return question, nil
	}
	return ad.Answer(ctx, question)
}
