package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/discriminator"
	"github.com/talyvor/lens/internal/embedder"
	"github.com/talyvor/lens/internal/poolsafety"
)

// `lens engcheck` — steps 1 and 2: do engineering rephrasings and engineering danger pairs
// SEPARATE at the configured threshold? Everything downstream depends on the answer.
func runEngCheck() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("engcheck: config: %w", err)
	}
	if cfg.OpenAIAPIKey == "" {
		return errors.New("engcheck: no embedding credential (LENS_OPENAI_API_KEY)")
	}
	emb := embedder.NewOpenAIEmbedder(cfg.OpenAIAPIKey, cfg.EmbeddingModel, cfg.EmbeddingBaseURL)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	th := cfg.SemanticThreshold

	// Positive controls first — a surprising number is only believable from a proven ruler.
	ctrl, err := poolsafety.ScorePairs(ctx, emb, poolsafety.PositiveControls())
	if err != nil {
		return err
	}
	fmt.Printf("POSITIVE CONTROLS\n")
	for _, c := range ctrl {
		v := "ok"
		if c.Pair.Name == "identical" && c.Similarity < 0.999 {
			v = "⚠ INSTRUMENT BROKEN — VOID THIS RUN"
		}
		fmt.Printf("  %-12s %.4f  %s\n", c.Pair.Name, c.Similarity, v)
	}

	re, err := poolsafety.ScorePairs(ctx, emb, poolsafety.EngineeringRephrasePairs())
	if err != nil {
		return err
	}
	dg, err := poolsafety.ScorePairs(ctx, emb, poolsafety.EngineeringDangerPairs())
	if err != nil {
		return err
	}

	asc := append([]poolsafety.PairScore(nil), re...)
	sort.Slice(asc, func(i, j int) bool { return asc[i].Similarity < asc[j].Similarity })
	pct := func(p float64) float64 { return asc[int(p*float64(len(asc)-1))].Similarity }

	var hits int
	for _, s := range re {
		if s.Similarity >= th {
			hits++
		}
	}
	fmt.Printf("\n1. ENGINEERING REPHRASINGS — %d pairs, model %s, threshold %.2f\n\n", len(re), cfg.EmbeddingModel, th)
	fmt.Printf("   hit rate: %d/%d = %.0f%%\n", hits, len(re), float64(hits)/float64(len(re))*100)
	fmt.Printf("   min %.4f · p10 %.4f · p25 %.4f · median %.4f · p75 %.4f · p90 %.4f · max %.4f\n\n",
		pct(0), pct(.10), pct(.25), pct(.50), pct(.75), pct(.90), pct(1))
	for _, s := range asc {
		m := "MISS"
		if s.Similarity >= th {
			m = "hit "
		}
		fmt.Printf("   %s %.4f  %-20s %q / %q\n", m, s.Similarity, s.Pair.Name, s.Pair.A, s.Pair.B)
	}

	fmt.Printf("\n2. ENGINEERING DANGER CORPUS — %d pairs that must NEVER pool\n\n", len(dg))
	for _, d := range dg {
		flag := ""
		if d.Similarity >= th {
			flag = "  ⚠⚠ WOULD FALSELY POOL AT THE CURRENT THRESHOLD"
		}
		fmt.Printf("   %.4f  %-20s %q / %q%s\n", d.Similarity, d.Pair.Name, d.Pair.A, d.Pair.B, flag)
	}
	fmt.Printf("\n   engineering danger ceiling: %.4f (%s)\n", dg[0].Similarity, dg[0].Pair.Name)
	fmt.Printf("   headroom below threshold:   %.4f\n", th-dg[0].Similarity)

	fmt.Print(poolsafety.SeparationTable(re, dg))

	// ⚠ STAGE 2A — WHERE CAN THE THRESHOLD GO ONCE THE GATE IS IN FRONT OF IT?
	//
	// With entity mismatches refused outright, similarity no longer has to carry the identity
	// judgement — it only has to judge topic, which is what it is good at. The binding constraint
	// stops being "the worst dangerous pair" and becomes "the worst dangerous pair THE GATE STILL
	// ALLOWS". Everything the gate refuses is off the board at every threshold.
	ho, err := poolsafety.ScorePairs(ctx, emb, poolsafety.HeldOutDangerPairs())
	if err != nil {
		return err
	}
	var allowedDanger, allowedReph []poolsafety.PairScore
	for _, d := range append(append([]poolsafety.PairScore{}, dg...), ho...) {
		if discriminator.Match(d.Pair.A, d.Pair.B) {
			allowedDanger = append(allowedDanger, d)
		}
	}
	for _, r := range re {
		if discriminator.Match(r.Pair.A, r.Pair.B) {
			allowedReph = append(allowedReph, r)
		}
	}
	fmt.Printf("\n=== STAGE 2A — POST-GATE SEPARATION (design %d + held-out %d danger pairs) ===\n\n", len(dg), len(ho))
	fmt.Printf("   danger pairs the gate ALLOWS through: %d of %d\n", len(allowedDanger), len(dg)+len(ho))
	for _, d := range allowedDanger {
		fmt.Printf("     %.4f  %-20s %q / %q\n", d.Similarity, d.Pair.Name, d.Pair.A, d.Pair.B)
	}
	fmt.Printf("   rephrasings the gate ALLOWS through: %d of %d\n", len(allowedReph), len(re))
	fmt.Print(poolsafety.SeparationTable(allowedReph, allowedDanger))
	return nil
}
