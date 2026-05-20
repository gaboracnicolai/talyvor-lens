package cache

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestExactCache(t *testing.T, ttl time.Duration) (*ExactCache, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	return NewExactCache(client, ttl), mr
}

func TestExactCache_GetMissReturnsNilNil(t *testing.T) {
	c, _ := newTestExactCache(t, time.Minute)

	got, err := c.Get(context.Background(), "openai", "gpt-4", "hello")
	if err != nil {
		t.Fatalf("expected nil error on miss, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil bytes on miss, got %q", got)
	}
}

func TestExactCache_SetThenGetReturnsSameBytes(t *testing.T) {
	c, _ := newTestExactCache(t, time.Minute)
	ctx := context.Background()
	want := []byte(`{"response":"hi"}`)

	if err := c.Set(ctx, "openai", "gpt-4", "hello", want); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := c.Get(ctx, "openai", "gpt-4", "hello")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestExactCache_ExpiredKeyReturnsNilNil(t *testing.T) {
	c, mr := newTestExactCache(t, time.Second)
	ctx := context.Background()

	if err := c.Set(ctx, "openai", "gpt-4", "hello", []byte("v")); err != nil {
		t.Fatalf("Set: %v", err)
	}

	mr.FastForward(2 * time.Second)

	got, err := c.Get(ctx, "openai", "gpt-4", "hello")
	if err != nil {
		t.Fatalf("expected nil error after expiry, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil after expiry, got %q", got)
	}
}

func TestExactCache_DifferentInputsProduceDifferentKeys(t *testing.T) {
	c, _ := newTestExactCache(t, time.Minute)

	cases := []struct {
		name       string
		a, b [3]string
	}{
		{"different prompt", [3]string{"openai", "gpt-4", "hello"}, [3]string{"openai", "gpt-4", "world"}},
		{"different provider", [3]string{"openai", "gpt-4", "x"}, [3]string{"anthropic", "gpt-4", "x"}},
		{"different model", [3]string{"openai", "gpt-4", "x"}, [3]string{"openai", "gpt-3.5", "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k1 := c.Key(tc.a[0], tc.a[1], tc.a[2])
			k2 := c.Key(tc.b[0], tc.b[1], tc.b[2])
			if k1 == k2 {
				t.Fatalf("expected different keys, got %s == %s", k1, k2)
			}
		})
	}
}

func TestExactCache_SamePromptAlwaysSameKey(t *testing.T) {
	c, _ := newTestExactCache(t, time.Minute)

	k1 := c.Key("openai", "gpt-4", "hello")
	k2 := c.Key("openai", "gpt-4", "hello")
	if k1 != k2 {
		t.Fatalf("expected deterministic key, got %s vs %s", k1, k2)
	}
	if !strings.HasPrefix(k1, "lens:exact:") {
		t.Fatalf("expected key prefix %q, got %s", "lens:exact:", k1)
	}
	// sha256 hex = 64 chars after the prefix
	if got := len(strings.TrimPrefix(k1, "lens:exact:")); got != 64 {
		t.Fatalf("expected 64-char sha256 hex suffix, got %d chars: %s", got, k1)
	}
}

func TestExactCache_KeyMatchesStoredEntry(t *testing.T) {
	c, mr := newTestExactCache(t, time.Minute)
	ctx := context.Background()

	if err := c.Set(ctx, "openai", "gpt-4", "hello", []byte("payload")); err != nil {
		t.Fatalf("Set: %v", err)
	}

	key := c.Key("openai", "gpt-4", "hello")
	stored, err := mr.Get(key)
	if err != nil {
		t.Fatalf("miniredis Get(%q): %v", key, err)
	}
	if stored != "payload" {
		t.Fatalf("redis stored %q at %s, want %q", stored, key, "payload")
	}
}
