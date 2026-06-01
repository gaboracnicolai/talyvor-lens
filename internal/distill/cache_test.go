package distill

import (
	"context"
	"strings"
	"testing"
)

// fakeCache is an in-memory Cache for testing the get-or-convert flow without
// Redis. It counts gets/sets so tests can prove a hit skips conversion + the
// Set call.
type fakeCache struct {
	store      map[string][]byte
	gets, sets int
}

func (f *fakeCache) Get(_ context.Context, hash, version string) ([]byte, error) {
	f.gets++
	return f.store[hash+"@"+version], nil // miss → nil
}

func (f *fakeCache) Set(_ context.Context, hash, version string, v []byte) error {
	f.sets++
	if f.store == nil {
		f.store = map[string][]byte{}
	}
	cp := make([]byte, len(v))
	copy(cp, v)
	f.store[hash+"@"+version] = cp
	return nil
}

func TestDistillWithCache_MissThenHit(t *testing.T) {
	ctx := context.Background()
	c := &fakeCache{}
	in := []byte("<html><body><h1>Hi</h1><p>there</p></body></html>")

	// MISS: converts + stores.
	r1, s1, err := DistillWithCache(ctx, c, in)
	if err != nil {
		t.Fatal(err)
	}
	if s1.CacheHit {
		t.Error("first call must be a miss (CacheHit=false)")
	}
	if c.sets != 1 {
		t.Errorf("miss must store once; sets=%d", c.sets)
	}
	if !strings.Contains(r1.Markdown, "# Hi") {
		t.Errorf("converted markdown unexpected: %q", r1.Markdown)
	}

	// HIT: same bytes → served from cache, no new Set, same Markdown.
	r2, s2, err := DistillWithCache(ctx, c, in)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.CacheHit {
		t.Error("second call must be a hit (CacheHit=true)")
	}
	if c.sets != 1 {
		t.Errorf("hit must NOT store again; sets=%d", c.sets)
	}
	if r2.Markdown != r1.Markdown || r2.Format != r1.Format {
		t.Errorf("hit must return the same Result: %q/%q vs %q/%q", r2.Markdown, r2.Format, r1.Markdown, r1.Format)
	}
}

func TestDistillWithCache_NilCache(t *testing.T) {
	// nil cache → still converts (no caching), savings still measured.
	r, s, err := DistillWithCache(context.Background(), nil, []byte("plain text here"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Format != FormatText || s.CacheHit {
		t.Errorf("nil-cache path: format=%q cacheHit=%v", r.Format, s.CacheHit)
	}
}

// Savings basis: raw = len(input)/4, distilled = len(markdown)/4 — the exact
// len/4 unit the gateway bills spend on (consistency is the whole point).
func TestSavings_LenOver4Basis(t *testing.T) {
	in := []byte("<html><body><h1>Title</h1><p>Hello <strong>world</strong>.</p>" +
		"<script>var noise = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa';</script></body></html>")
	r, s, err := DistillWithCache(context.Background(), nil, in)
	if err != nil {
		t.Fatal(err)
	}
	if s.InputTokensRaw != len(in)/4 {
		t.Errorf("InputTokensRaw=%d, want len(in)/4=%d", s.InputTokensRaw, len(in)/4)
	}
	if s.InputTokensDistilled != len(r.Markdown)/4 {
		t.Errorf("InputTokensDistilled=%d, want len(md)/4=%d", s.InputTokensDistilled, len(r.Markdown)/4)
	}
	if s.TokensSaved != s.InputTokensRaw-s.InputTokensDistilled {
		t.Errorf("TokensSaved=%d, want raw-distilled=%d", s.TokensSaved, s.InputTokensRaw-s.InputTokensDistilled)
	}
	// Verbose HTML (tags + a stripped script) → distillation is a net win.
	if s.TokensSaved <= 0 {
		t.Errorf("verbose HTML should save tokens; got %d (raw=%d distilled=%d)", s.TokensSaved, s.InputTokensRaw, s.InputTokensDistilled)
	}
}

// A text-less PDF yields NeedsVision (no Markdown) → savings must be ZERO, not
// "saved the whole input" (which would be a fake number).
func TestSavings_NeedsVisionZero(t *testing.T) {
	r, s, err := DistillWithCache(context.Background(), nil, buildPDF()) // no text → NeedsVision
	if err != nil {
		t.Fatal(err)
	}
	if !r.NeedsVision {
		t.Fatalf("expected NeedsVision for text-less PDF; md=%q", r.Markdown)
	}
	if s.TokensSaved != 0 {
		t.Errorf("NeedsVision must report 0 tokens saved (no Markdown delivered); got %d", s.TokensSaved)
	}
}

// The version is part of the addressing: a different ConverterVersion must not
// serve an entry stored under the old version (no stale Markdown).
func TestCache_VersionIsolates(t *testing.T) {
	ctx := context.Background()
	c := &fakeCache{}
	in := []byte("<p>hi</p>")
	hash := ContentHash(in)

	// Seed an entry under a DIFFERENT version.
	_ = c.Set(ctx, hash, "old-version", []byte(`{"result":{"Markdown":"STALE","Format":"html"}}`))

	// DistillWithCache uses ConverterVersion, so it must MISS the stale entry,
	// convert fresh, and not return "STALE".
	r, s, err := DistillWithCache(ctx, c, in)
	if err != nil {
		t.Fatal(err)
	}
	if s.CacheHit {
		t.Error("entry under a different version must not be a hit")
	}
	if strings.Contains(r.Markdown, "STALE") {
		t.Errorf("must not serve stale Markdown from an old converter version: %q", r.Markdown)
	}
}

func TestContentHash_Deterministic(t *testing.T) {
	a := ContentHash([]byte("same"))
	b := ContentHash([]byte("same"))
	d := ContentHash([]byte("different"))
	if a != b {
		t.Error("ContentHash must be deterministic")
	}
	if a == d {
		t.Error("different content must hash differently")
	}
	if len(a) != 64 {
		t.Errorf("sha256 hex should be 64 chars, got %d", len(a))
	}
}

// The cache key incorporates the TIER: faithful vs outline of the SAME document
// are distinct outputs and must not collide in the cache (stage-4 correctness).
func TestCache_TierInKey(t *testing.T) {
	ctx := context.Background()
	c := &fakeCache{}
	in := mustRead(t, "sample.html")

	rF, _, _ := DistillWithCache(ctx, c, in, WithTier(TierFaithful))
	rO, _, _ := DistillWithCache(ctx, c, in, WithTier(TierOutline))
	if c.sets != 2 {
		t.Fatalf("faithful + outline of the same doc must store 2 DISTINCT entries; sets=%d", c.sets)
	}
	if rF.Markdown == rO.Markdown {
		t.Error("faithful and outline markdown must differ")
	}

	// Re-fetch each tier → both HIT (no new stores), each returns its own tier.
	rF2, sF2, _ := DistillWithCache(ctx, c, in, WithTier(TierFaithful))
	rO2, sO2, _ := DistillWithCache(ctx, c, in, WithTier(TierOutline))
	if c.sets != 2 {
		t.Errorf("re-fetch must hit, not store; sets=%d", c.sets)
	}
	if !sF2.CacheHit || !sO2.CacheHit {
		t.Errorf("re-fetch must be hits: faithful=%v outline=%v", sF2.CacheHit, sO2.CacheHit)
	}
	if rF2.Markdown != rF.Markdown || rF2.Tier != TierFaithful {
		t.Errorf("faithful hit served wrong entry: %q tier=%q", rF2.Markdown, rF2.Tier)
	}
	if rO2.Markdown != rO.Markdown || rO2.Tier != TierOutline {
		t.Errorf("outline hit served wrong entry: %q tier=%q", rO2.Markdown, rO2.Tier)
	}
}
