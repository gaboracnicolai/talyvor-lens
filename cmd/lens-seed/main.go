// Command lens-seed is the operator tool that pre-seeds the warm-start cache with Talyvor-OWNED
// entries so a fresh deployment serves cache hits on day one. It reads a JSONL file of seed entries
// and writes them via the public cache store methods (internal/seedcache), with the owner HARDCODED
// to economy.TalyvorSeedWorkspace — never a flag. Seeded entries provably mint nothing (the seed
// owner is never earn_verified → MayEarn false → both royalty mints roll back at the held-ledger
// chokepoint). NOT a serve-path component; run manually by an operator (no flag gates serving —
// seeds only serve cross-tenant when the existing pooling flags are on).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/embedder"
	"github.com/talyvor/lens/internal/seedcache"
)

// seedEntry is one JSONL line. For distill, Prompt carries the document content (content-hashed)
// and Response the OCR markdown; InputTokens/OutputTokens are the vision-OCR cost basis.
type seedEntry struct {
	Kind         string `json:"kind"` // exact | semantic | distill
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Prompt       string `json:"prompt"`
	Response     string `json:"response"`
	InputTokens  int    `json:"input_tokens,omitempty"`
	OutputTokens int    `json:"output_tokens,omitempty"`
}

func main() {
	file := flag.String("file", "", "path to a JSONL file of seed entries {kind, provider, model, prompt, response}")
	dryRun := flag.Bool("dry-run", false, "parse + validate without writing")
	flag.Parse()
	if *file == "" {
		fatalf("-file is required")
	}

	cfg, err := config.Load()
	if err != nil {
		fatalf("config: %v", err)
	}
	ctx := context.Background()

	redisOpts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		fatalf("redis url: %v", err)
	}
	rc := redis.NewClient(redisOpts)
	defer func() { _ = rc.Close() }()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		fatalf("db: %v", err)
	}
	defer pool.Close()
	emb := embedder.NewOpenAIEmbedder(cfg.OpenAIAPIKey, cfg.EmbeddingModel, cfg.EmbeddingBaseURL)

	seeder := seedcache.NewSeeder(
		cache.NewExactCache(rc, cfg.MaxCacheTTL),
		cache.NewSemanticCache(pool, emb, cfg.SemanticThreshold, cfg.SemanticCacheRetention),
		cache.NewDistillCache(rc, cfg.MaxCacheTTL),
		emb,
	)

	f, err := os.Open(*file)
	if err != nil {
		fatalf("open %s: %v", *file, err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024) // allow large response/document lines
	var seeded, line int
	for sc.Scan() {
		line++
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e seedEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			fatalf("line %d: bad JSON: %v", line, err)
		}
		if *dryRun {
			if e.Kind != "exact" && e.Kind != "semantic" && e.Kind != "distill" {
				fatalf("line %d: unknown kind %q", line, e.Kind)
			}
			seeded++
			continue
		}
		if err := seed(ctx, seeder, e); err != nil {
			fatalf("line %d (%s): %v", line, e.Kind, err)
		}
		seeded++
	}
	if err := sc.Err(); err != nil {
		fatalf("read: %v", err)
	}
	fmt.Printf("lens-seed: %d entries seeded as owner %q (dry-run=%v)\n", seeded, seedcache.Owner, *dryRun)
}

func seed(ctx context.Context, s *seedcache.Seeder, e seedEntry) error {
	switch e.Kind {
	case "exact":
		return s.SeedExact(ctx, e.Provider, e.Model, e.Prompt, []byte(e.Response))
	case "semantic":
		return s.SeedSemantic(ctx, e.Provider, e.Model, e.Prompt, []byte(e.Response))
	case "distill":
		return s.SeedDistill(ctx, e.Model, []byte(e.Prompt), e.Response, e.InputTokens, e.OutputTokens)
	default:
		return fmt.Errorf("unknown kind %q (want exact|semantic|distill)", e.Kind)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "lens-seed: "+format+"\n", args...)
	os.Exit(1)
}
