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

const exactKeyPrefix = "lens:exact:"

// ErrCacheMiss is returned by other cache layers (e.g. semantic) to signal
// a miss. ExactCache.Get does not return this error; it returns (nil, nil)
// on miss, consistent with the spec.
var ErrCacheMiss = errors.New("cache miss")

type ExactCache struct {
	client *redis.Client
	ttl    time.Duration
}

func NewExactCache(client *redis.Client, ttl time.Duration) *ExactCache {
	return &ExactCache{client: client, ttl: ttl}
}

func (c *ExactCache) Key(provider, model, prompt string) string {
	sum := sha256.Sum256([]byte(provider + ":" + model + ":" + prompt))
	return exactKeyPrefix + hex.EncodeToString(sum[:])
}

// Get returns the cached response bytes for the given request.
// A cache miss returns (nil, nil) — not an error.
func (c *ExactCache) Get(ctx context.Context, provider, model, prompt string) ([]byte, error) {
	b, err := c.client.Get(ctx, c.Key(provider, model, prompt)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	metrics.CacheHitsTotal.WithLabelValues("exact").Inc()
	return b, nil
}

func (c *ExactCache) Set(ctx context.Context, provider, model, prompt string, response []byte) error {
	return c.client.Set(ctx, c.Key(provider, model, prompt), response, c.ttl).Err()
}
