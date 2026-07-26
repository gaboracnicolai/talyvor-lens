package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/embedder"
	"github.com/talyvor/lens/internal/poolsafety"
)

// runPoolCheck runs the cross-tenant pooling safety preflight against the CONFIGURED
// embedder and threshold — not a copy of them, so the thing measured is the thing that
// serves. Exits non-zero (via the returned error) when an unrelated pair reaches the
// threshold.
//
// Why this exists: whether shared prompt boilerplate can push two unrelated tenants past
// the pooling threshold depends on the embedding model. Measured on the same pair of
// unrelated codebases, text-embedding-3-small scores 0.69 (safe), all-MiniLM 0.84, and
// bge-small 0.985 (would serve across tenants). LENS_EMBEDDING_MODEL is an ordinary
// operational knob; nothing connected it to that outcome until now.
func runPoolCheck() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("poolcheck: config: %w", err)
	}
	if cfg.OpenAIAPIKey == "" {
		return errors.New("poolcheck: no embedding credential configured (LENS_OPENAI_API_KEY); " +
			"cannot measure, and an unmeasured configuration must not be called safe")
	}

	emb := embedder.NewOpenAIEmbedder(cfg.OpenAIAPIKey, cfg.EmbeddingModel, cfg.EmbeddingBaseURL)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	fmt.Printf("embedding model: %s\nthreshold:       %.2f\npooling enabled: %v\n\n",
		cfg.EmbeddingModel, cfg.SemanticThreshold, cfg.CachePoolableEnabled)

	rep, err := poolsafety.Check(ctx, emb, cfg.SemanticThreshold)
	if err != nil {
		return fmt.Errorf("poolcheck: %w", err)
	}
	fmt.Print(rep.String())

	if !rep.OK {
		if !cfg.CachePoolableEnabled {
			fmt.Fprintln(os.Stderr,
				"\nNOTE: cross-tenant pooling is currently DISABLED, so this is not live today —\n"+
					"but this configuration must not be enabled until the above is resolved.")
		}
		return errors.New("pool-safety check FAILED: unrelated prompts reach the pooling threshold")
	}
	return nil
}
