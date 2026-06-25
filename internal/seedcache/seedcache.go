// Package seedcache writes Talyvor-OWNED warm-start cache entries so a fresh deployment serves
// cache hits on day one. Every write stamps economy.TalyvorSeedWorkspace as the contributor/owner
// — HARDCODED, so a seed can never carry a tenant owner.
//
// ZERO-MINT BY CONSTRUCTION: seeded entries serve like any pooled entry, but provably mint
// nothing. The seed owner is never earn_verified and never has an lxc_purchase, so
// earnverify.MayEarn returns false for it → both royalty mint paths (cache poolroyalty/minter,
// distill poolroyalty/distill_minter) roll back at the shared held-ledger verifyEarn chokepoint
// (held_ledger.go heldInner → mint_gate verifyEarn → MayEarn). This package writes ONLY via the
// public store methods (no raw SQL); it never sets earn_verified and never inserts an lxc_purchase.
package seedcache

import (
	"context"
	"fmt"

	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/distill"
	"github.com/talyvor/lens/internal/economy"
)

// Owner is the immutable seed owner stamped on every seeded entry. Hardcoded (never a parameter)
// so a seed can never be written under a tenant id. economy.TalyvorSeedWorkspace is, by contract,
// never earn_verified — that is the single fact the zero-mint guarantee rests on.
const Owner = economy.TalyvorSeedWorkspace

// Seeder owns the public cache write surfaces. A nil cache disables that kind (the operator may
// run with only exact, etc.). It holds no ledger and reaches no mint path.
type Seeder struct {
	exact    *cache.ExactCache
	semantic *cache.SemanticCache
	distill  *cache.DistillCache
	embedder cache.Embedder
}

// NewSeeder wires the write surfaces. embedder is required only for semantic seeding (the SAME
// Embedder the production semantic cache uses, so seeded vectors match the serve geometry).
func NewSeeder(exact *cache.ExactCache, semantic *cache.SemanticCache, dc *cache.DistillCache, embedder cache.Embedder) *Seeder {
	return &Seeder{exact: exact, semantic: semantic, distill: dc, embedder: embedder}
}

// SeedExact writes a POOLED exact-cache entry owned by the seed workspace — keyed via
// cache.PooledPromptKey so it lands in the cross-tenant keyspace the serve path reads.
func (s *Seeder) SeedExact(ctx context.Context, provider, model, prompt string, response []byte) error {
	if s == nil || s.exact == nil {
		return fmt.Errorf("seedcache: exact cache not configured")
	}
	return s.exact.SetWithOwner(ctx, provider, model, cache.PooledPromptKey(prompt), Owner, response)
}

// SeedSemantic embeds the RAW prompt (the same Embedder the cache uses) and writes a POOLED
// semantic row owned by the seed workspace. On serve it must clear the SAME cosine-similarity
// threshold as any other entry — no special-casing, so a seed can never serve a low-quality match.
func (s *Seeder) SeedSemantic(ctx context.Context, provider, model, prompt string, response []byte) error {
	if s == nil || s.semantic == nil {
		return fmt.Errorf("seedcache: semantic cache not configured")
	}
	if s.embedder == nil {
		return fmt.Errorf("seedcache: semantic seeding needs an embedder")
	}
	vec, err := s.embedder.Embed(ctx, prompt)
	if err != nil {
		return fmt.Errorf("seedcache: embed: %w", err)
	}
	return s.semantic.SetPooled(ctx, provider, model, cache.PooledPromptKey(prompt), Owner, response, vec)
}

// SeedDistill writes a POOLED distill-OCR entry owned by the seed workspace: it content-hashes the
// document, marshals the OCR result + its vision-cost basis, and writes under distill.PoolMarker
// (the cross-tenant keyspace) at the model's OCR cache version. visionInputTokens/visionOutputTokens
// are the OCR sub-call cost the cached artifact records (the avoided-COGS basis on reuse).
func (s *Seeder) SeedDistill(ctx context.Context, model string, document []byte, markdown string, visionInputTokens, visionOutputTokens int) error {
	if s == nil || s.distill == nil {
		return fmt.Errorf("seedcache: distill cache not configured")
	}
	res := distill.Result{Markdown: markdown, NeedsVision: false, Method: distill.MethodVisionOCR}
	sav := distill.Savings{VisionModel: model, VisionInputTokens: visionInputTokens, VisionOutputTokens: visionOutputTokens}
	b, err := distill.MarshalCachedOCR(res, sav)
	if err != nil {
		return fmt.Errorf("seedcache: marshal OCR: %w", err)
	}
	return s.distill.SetWithOwner(ctx, distill.PoolMarker+distill.ContentHash(document), distill.OCRCacheVersion(model), Owner, b)
}
