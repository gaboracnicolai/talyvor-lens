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
	return exactKeyPrefix + PromptHash(provider, model, prompt)
}

// PromptHash is the digest an entry is stored under: the exact key's suffix and, for the same key
// material, the semantic row's prompt_hash — so one hash names both copies of one stored answer.
func PromptHash(provider, model, prompt string) string {
	sum := sha256.Sum256([]byte(provider + ":" + model + ":" + prompt))
	return hex.EncodeToString(sum[:])
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

// ownerKey is the parallel Redis key used by SetWithOwner /
// GetWithOwner. We deliberately keep it separate from the
// response key so the existing Get/Set surface stays untouched
// and binary-compatible. Mining is opt-in — callers pay the
// extra round-trip only when they care.
func (c *ExactCache) ownerKey(provider, model, prompt string) string {
	return c.Key(provider, model, prompt) + ":owner"
}

// SetWithOwner caches the response and stamps `workspaceID` as
// the contributing owner in a parallel key. Both keys share the
// same TTL so they age out together.
func (c *ExactCache) SetWithOwner(ctx context.Context, provider, model, prompt, workspaceID string, response []byte) error {
	if err := c.Set(ctx, provider, model, prompt, response); err != nil {
		return err
	}
	if workspaceID == "" {
		return nil
	}
	return c.client.Set(ctx, c.ownerKey(provider, model, prompt), workspaceID, c.ttl).Err()
}

// GetWithOwner returns the cached response *and* the workspace
// that contributed it (empty string when no owner was recorded
// — older entries from before owner tracking landed). A cache
// miss is (nil, "", nil).
func (c *ExactCache) GetWithOwner(ctx context.Context, provider, model, prompt string) ([]byte, string, error) {
	body, err := c.Get(ctx, provider, model, prompt)
	if err != nil || body == nil {
		return body, "", err
	}
	owner, err := c.client.Get(ctx, c.ownerKey(provider, model, prompt)).Result()
	if errors.Is(err, redis.Nil) {
		return body, "", nil
	}
	if err != nil {
		// Owner read failure is non-fatal — surface the body
		// regardless so the request still completes.
		return body, "", nil
	}
	return body, owner, nil
}

// DeleteIfDigest removes the entry stored under hash (a PromptHash) and its owner stamp, but only
// while it still holds the answer whose sha256 is digest — an answer written there since is kept.
// B23.1: a thumbs-down removes the answer that was served, never a newer one.
func (c *ExactCache) DeleteIfDigest(ctx context.Context, hash, digest string) (bool, error) {
	key := exactKeyPrefix + hash
	b, err := c.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if AnswerDigest(b) != digest {
		return false, nil
	}
	return true, c.client.Del(ctx, key, key+":owner").Err()
}

// AnswerDigest is the hex sha256 of a stored answer's bytes.
func AnswerDigest(answer []byte) string {
	sum := sha256.Sum256(answer)
	return hex.EncodeToString(sum[:])
}

const servedKeyPrefix = "lens:served:"

// RememberServed records ref — which stored answer answered request requestID of workspace wsID —
// for ttl, so a thumbs-down on that request can find it (B23.1). The key is per workspace, so a
// workspace can only ever reach the answers it was served.
func (c *ExactCache) RememberServed(ctx context.Context, wsID, requestID string, ref []byte, ttl time.Duration) error {
	return c.client.Set(ctx, servedKeyPrefix+wsID+":"+requestID, ref, ttl).Err()
}

// ServedRef returns what RememberServed recorded for the request, or nil when nothing was (or it
// has expired).
func (c *ExactCache) ServedRef(ctx context.Context, wsID, requestID string) ([]byte, error) {
	b, err := c.client.Get(ctx, servedKeyPrefix+wsID+":"+requestID).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	return b, err
}
