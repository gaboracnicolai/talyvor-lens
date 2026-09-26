package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/discriminator"
	"github.com/talyvor/lens/internal/embedder"
	"github.com/talyvor/lens/internal/poolsafety"
)

// runPoolCheck runs the cross-tenant pooling safety preflight against the CONFIGURED
// embedder — not a copy of it, so the thing measured is the thing that serves — and
// attests the SAFETY FLOOR it measures (B9.4): the lowest threshold at which no committed
// danger pair passes the serving predicate. Exits non-zero (via the returned error) when
// the configured threshold is below that floor.
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

	// B9.4 — attest the MEASURED floor, not whatever threshold happens to be live: the lowest threshold at
	// which no committed danger pair passes the serving predicate. Boot then refuses pooling whenever the
	// live threshold is below it (Attestation.MatchesLive).
	floor, worstPair, worstScore, err := poolsafety.SafetyFloor(ctx, emb, discriminator.Match)
	if err != nil {
		return fmt.Errorf("poolcheck: %w", err)
	}
	ok := cfg.SemanticThreshold >= floor
	fmt.Printf("safety floor:    %.4f (worst pair %s at %.6f)\nlive threshold:  %.4f — %s\n",
		floor, worstPair, worstScore, cfg.SemanticThreshold, map[bool]string{true: "above the floor", false: "BELOW the floor"}[ok])

	// Record the floor so the gateway can bind to it at boot. Written ONLY when the live threshold is
	// above it, so a stored attestation always means "this configuration was measured safe".
	if ok {
		pool, perr := pgxpool.New(ctx, cfg.DatabaseURL)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "\nWARNING: check passed but the attestation could not be recorded (db connect: %v).\n"+
				"Pooling will stay OFF at boot until it is.\n", perr)
			return nil
		}
		defer pool.Close()
		if rerr := poolsafety.Record(ctx, attestationWriter{pool}, poolsafety.Attestation{
			EmbeddingModel: cfg.EmbeddingModel,
			Threshold:      floor,
			WorstPair:      worstPair,
			WorstScore:     worstScore,
		}); rerr != nil {
			fmt.Fprintf(os.Stderr, "\nWARNING: check passed but the attestation could not be recorded (%v).\n"+
				"Pooling will stay OFF at boot until it is.\n", rerr)
			return nil
		}
		fmt.Printf("\nattested: %s, floor %.4f — pooling permitted at boot while the live threshold is ≥ %.4f\n",
			cfg.EmbeddingModel, floor, floor)
		return nil
	}
	if !cfg.CachePoolableEnabled {
		fmt.Fprintln(os.Stderr,
			"\nNOTE: cross-tenant pooling is currently DISABLED, so this is not live today —\n"+
				"but this configuration must not be enabled until the above is resolved.")
	}
	return fmt.Errorf("pool-safety check FAILED: the live threshold %.4f is below the safety floor %.4f (%s serves at %.4f)",
		cfg.SemanticThreshold, floor, worstPair, worstScore)
}

// attestationWriter adapts a pgxpool to poolsafety.Writer.
type attestationWriter struct{ pool *pgxpool.Pool }

func (w attestationWriter) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := w.pool.Exec(ctx, sql, args...)
	return err
}
