package cache

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/talyvor/lens/internal/distill"
)

// Compile-time proof that DistillCache satisfies the seam internal/distill
// depends on — i.e. the reuse actually wires up. (cache imports distill here
// in test only; distill does not import cache, so there is no cycle.)
var _ distill.Cache = (*DistillCache)(nil)

func newTestDistillCache(t *testing.T, ttl time.Duration) (*DistillCache, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return NewDistillCache(client, ttl), mr
}

func TestDistillCache_RoundTripAndMiss(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestDistillCache(t, time.Minute)

	if b, err := c.Get(ctx, "hash1", "1"); err != nil || b != nil {
		t.Fatalf("miss must be (nil, nil); got %q, %v", b, err)
	}
	if err := c.Set(ctx, "hash1", "1", []byte("value")); err != nil {
		t.Fatal(err)
	}
	if b, err := c.Get(ctx, "hash1", "1"); err != nil || string(b) != "value" {
		t.Fatalf("hit must return value; got %q, %v", b, err)
	}
}

func TestDistillCache_HashAndVersionNamespacing(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestDistillCache(t, time.Minute)
	_ = c.Set(ctx, "h", "v1", []byte("one"))

	if b, _ := c.Get(ctx, "h", "v2"); b != nil {
		t.Error("different version must not hit (no stale-converter serving)")
	}
	if b, _ := c.Get(ctx, "h2", "v1"); b != nil {
		t.Error("different content hash must not hit")
	}
	if b, _ := c.Get(ctx, "h", "v1"); string(b) != "one" {
		t.Error("same (hash, version) must hit")
	}
	if k := c.Key("h", "v1"); !strings.HasPrefix(k, "lens:distill:") {
		t.Errorf("key must be namespaced under lens:distill:, got %q", k)
	}
}

func TestDistillCache_TTLExpiry(t *testing.T) {
	ctx := context.Background()
	c, mr := newTestDistillCache(t, time.Minute)
	_ = c.Set(ctx, "h", "1", []byte("v"))
	mr.FastForward(2 * time.Minute) // past the TTL
	if b, _ := c.Get(ctx, "h", "1"); b != nil {
		t.Error("entry should have expired after the TTL")
	}
}
