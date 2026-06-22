package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/talyvor/lens/internal/metrics"
)

// distillKind labels a cache lookup by its keyspace, read from the version
// discriminator: the vision-OCR result cache versions with an "ocr:" prefix
// (distill.ocrCacheVersion), the conversion cache with a leading digit. Lets the
// one Get/Set surface serve both keyspaces while keeping the metric distinct.
func distillKind(version string) string {
	if strings.HasPrefix(version, "ocr:") {
		return "ocr"
	}
	return "conversion"
}

const distillKeyPrefix = "lens:distill:"

// DistillCache is the conversion cache for DISTILL (stage 2). It reuses the
// same Redis infrastructure + conventions as ExactCache, but is a CONTENT-
// addressed cache (keyed by the document's content hash + the converter
// version) in its own "lens:distill:" namespace, with its own hit/miss metric
// — it is NOT the prompt-keyed LLM cache and must not pollute that metric.
//
// Keying on the converter version means a converter change (bump
// distill.ConverterVersion) lands on fresh keys, so stale Markdown from an old
// converter is never served. The value is the serialized conversion Result;
// serialization is the caller's concern (this layer is bytes-in/bytes-out,
// like ExactCache).
type DistillCache struct {
	client *redis.Client
	ttl    time.Duration
}

func NewDistillCache(client *redis.Client, ttl time.Duration) *DistillCache {
	return &DistillCache{client: client, ttl: ttl}
}

// Key is the Redis key for a (contentHash, version) pair. Hashing the pair
// keeps keys fixed-length and namespaces them under lens:distill:.
//
// INJECTIVITY INVARIANT: the "version:contentHash" pre-image is unambiguous
// only because contentHash is fixed-length hex (never contains ':') AND version
// is a controlled constant (distill.ConverterVersion). If version ever became
// caller-supplied or could contain ':', length-prefix or domain-separate the
// components instead — otherwise distinct pairs could collapse to one key and
// serve cross-version Markdown.
func (c *DistillCache) Key(contentHash, version string) string {
	sum := sha256.Sum256([]byte(version + ":" + contentHash))
	return distillKeyPrefix + hex.EncodeToString(sum[:])
}

// Get returns the cached Result bytes for (contentHash, version). A miss is
// (nil, nil) — not an error — matching ExactCache. Increments the bounded
// distill cache hit/miss metric.
func (c *DistillCache) Get(ctx context.Context, contentHash, version string) ([]byte, error) {
	b, err := c.client.Get(ctx, c.Key(contentHash, version)).Bytes()
	if errors.Is(err, redis.Nil) {
		metrics.DistillCache("miss", distillKind(version))
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	metrics.DistillCache("hit", distillKind(version))
	return b, nil
}

// Set stores the serialized Result for (contentHash, version) under the shared
// TTL.
func (c *DistillCache) Set(ctx context.Context, contentHash, version string, value []byte) error {
	return c.client.Set(ctx, c.Key(contentHash, version), value, c.ttl).Err()
}

// ownerKey is the parallel Redis key holding the contributing workspace for a
// POOLED distill artifact (the cross-tenant-sharing analogue of
// ExactCache.ownerKey). Kept separate from the value key so the plain Get/Set
// surface stays binary-compatible — only the pooled keyspace pays for it.
func (c *DistillCache) ownerKey(contentHash, version string) string {
	return c.Key(contentHash, version) + ":owner"
}

// SetWithOwner stores the artifact AND stamps `workspaceID` as the contributing
// owner in a parallel key (same TTL). Used only for the pooled (shared)
// keyspace, where the serve-time consent check needs the owner's identity to
// verify the owner's own opt-in (PoolabilityGate.MaybeAllowPooledHit).
func (c *DistillCache) SetWithOwner(ctx context.Context, contentHash, version, workspaceID string, value []byte) error {
	if err := c.Set(ctx, contentHash, version, value); err != nil {
		return err
	}
	if workspaceID == "" {
		return nil
	}
	return c.client.Set(ctx, c.ownerKey(contentHash, version), workspaceID, c.ttl).Err()
}

// GetWithOwner returns the pooled artifact AND the workspace that contributed
// it (empty when no owner was recorded — e.g. a pre-feature entry, which the
// consent gate then refuses). A miss is (nil, "", nil).
func (c *DistillCache) GetWithOwner(ctx context.Context, contentHash, version string) ([]byte, string, error) {
	body, err := c.Get(ctx, contentHash, version)
	if err != nil || body == nil {
		return body, "", err
	}
	owner, err := c.client.Get(ctx, c.ownerKey(contentHash, version)).Result()
	if errors.Is(err, redis.Nil) || err != nil {
		// No/failed owner read is non-fatal: surface the body with an empty
		// owner, which the consent gate treats as not-poolable (safe default).
		return body, "", nil
	}
	return body, owner, nil
}
