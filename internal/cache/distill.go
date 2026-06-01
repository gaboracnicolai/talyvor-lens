package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/talyvor/lens/internal/metrics"
)

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
		metrics.DistillCache("miss")
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	metrics.DistillCache("hit")
	return b, nil
}

// Set stores the serialized Result for (contentHash, version) under the shared
// TTL.
func (c *DistillCache) Set(ctx context.Context, contentHash, version string, value []byte) error {
	return c.client.Set(ctx, c.Key(contentHash, version), value, c.ttl).Err()
}
