package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/embedder"
	"github.com/talyvor/lens/internal/poolsafety"
)

// `lens rephrasecheck` — the utility half of `lens poolcheck`.
//
// poolcheck measures the SAFETY ceiling: how close two UNRELATED prompts get, and therefore
// whether cross-tenant serving is safe at the configured threshold. This measures the UTILITY
// floor: how close two people asking the SAME question in different words actually get, and
// therefore whether the pool serves anybody.
//
// Both numbers are properties of (embedding model × threshold), and a deployment that knows one
// and assumes the other is guessing about the thing its product promises. Run them together.
//
// ⚠ IT CHANGES NOTHING. No threshold is written, no attestation is recorded, no configuration is
// touched. It prints numbers. Choosing a threshold from them is a judgement about the trade
// between "serves more rephrasings" and "the margin that makes cross-tenant serving safe", and
// that judgement is not this command's to make.
func runRephraseCheck() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("rephrasecheck: config: %w", err)
	}
	if cfg.OpenAIAPIKey == "" {
		return errors.New("rephrasecheck: no embedding credential configured (LENS_OPENAI_API_KEY); " +
			"the hit rate is a property of the live embedder and cannot be estimated without it")
	}

	emb := embedder.NewOpenAIEmbedder(cfg.OpenAIAPIKey, cfg.EmbeddingModel, cfg.EmbeddingBaseURL)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	rep, err := poolsafety.MeasureRephrase(ctx, emb, cfg.EmbeddingModel, cfg.SemanticThreshold)
	if err != nil {
		return fmt.Errorf("rephrasecheck: %w", err)
	}

	// The unrelated ceiling, from poolcheck's own corpus and its own code — so the two halves of
	// the picture are measured by the same instrument on the same configuration, and the headroom
	// figure is not assembled from two different runs.
	safety, err := poolsafety.Check(ctx, emb, cfg.SemanticThreshold)
	if err != nil {
		return fmt.Errorf("rephrasecheck: safety half: %w", err)
	}
	rep.UnrelatedCeiling = safety.Worst.Similarity
	rep.UnrelatedCeilingPair = safety.Worst.Pair.Name
	rep.UnrelatedPairsChecked = len(safety.Pairs)

	fmt.Print(rep.String())

	// ⚠ THE PRIVATE/POOLED ASYMMETRY, measured rather than asserted. See MeasurePrefixLift.
	lift, err := poolsafety.MeasurePrefixLift(ctx, emb, "uibdb4r4rmi6erscdvpj6rgv7u6", cfg.SemanticThreshold)
	if err != nil {
		return fmt.Errorf("rephrasecheck: prefix lift: %w", err)
	}
	fmt.Print(lift.String())

	// ⚠ POSITIVE CONTROLS LAST, PRINTED ALWAYS. A 0% hit rate and a silently-broken embedder are
	// indistinguishable in the numbers above. Identical text must score ~1.0 or the run is void.
	ctrl, err := poolsafety.ScorePairs(ctx, emb, poolsafety.PositiveControls())
	if err != nil {
		return fmt.Errorf("rephrasecheck: positive controls: %w", err)
	}
	fmt.Printf("\nPOSITIVE CONTROLS — the ruler, checked before its readings are believed\n\n")
	for _, c := range ctrl {
		verdict := "ok"
		if c.Pair.Name == "identical" && c.Similarity < 0.999 {
			verdict = "⚠ INSTRUMENT BROKEN — identical text must score 1.0; VOID THIS RUN"
		}
		fmt.Printf("  %-12s %.4f  %s\n", c.Pair.Name, c.Similarity, verdict)
	}

	// ⚠ THE CONSUMER-SIDE CEILING. Corpus() measures developer traffic with shared preambles; the
	// consumer hazard is one word changed and a different correct answer. See ConsumerUnrelatedPairs.
	cons, err := poolsafety.ScorePairs(ctx, emb, poolsafety.ConsumerUnrelatedPairs())
	if err != nil {
		return fmt.Errorf("rephrasecheck: consumer unrelated: %w", err)
	}
	fmt.Printf("\nCONSUMER UNRELATED CEILING — pairs that must NEVER pool (different correct answer)\n\n")
	for _, c := range cons {
		flag := ""
		if c.Similarity >= cfg.SemanticThreshold {
			flag = "  ⚠⚠ WOULD FALSELY POOL AT THE CURRENT THRESHOLD"
		}
		fmt.Printf("  %.4f  %-18s %q / %q%s\n", c.Similarity, c.Pair.Name, c.Pair.A, c.Pair.B, flag)
	}
	if len(cons) > 0 {
		fmt.Printf("\n  consumer unrelated ceiling: %.4f (%s)\n", cons[0].Similarity, cons[0].Pair.Name)
		fmt.Printf("  headroom below the current threshold: %.4f\n", cfg.SemanticThreshold-cons[0].Similarity)
		fmt.Printf("  ⚠ compare against the rephrase distribution above: any rephrasing scoring BELOW\n")
		fmt.Printf("    this number cannot be served by ANY threshold without also serving these.\n")
	}

	// The table that makes the trade-off decidable — served vs admitted at every candidate
	// threshold, so the choice is visible rather than argued. It recommends nothing.
	fmt.Print(poolsafety.SeparationTable(rep.Scores, cons))
	return nil
}
