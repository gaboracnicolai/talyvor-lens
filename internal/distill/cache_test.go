package distill

import (
	"context"
	"encoding/json"
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

// Binary-origin formats (PDF/DOCX/XLSX) carry raw bytes that are NOT text
// tokens; text-ish formats are tokens a model could have been sent.
func TestFormat_IsBinaryOrigin(t *testing.T) {
	for _, f := range []Format{FormatPDF, FormatDOCX, FormatXLSX} {
		if !f.IsBinaryOrigin() {
			t.Errorf("%s must be binary-origin", f)
		}
	}
	for _, f := range []Format{FormatHTML, FormatCSV, FormatJSON, FormatXML, FormatText, FormatUnknown} {
		if f.IsBinaryOrigin() {
			t.Errorf("%s must NOT be binary-origin", f)
		}
	}
}

// Binary-origin at FAITHFUL tier: the binary→text step is a SIZE reduction
// (bytes), NOT a token saving. There must be NO phantom len(bytes)/4 token
// saving — the raw baseline is the faithful-text token count, so the tier delta
// (0 at faithful) is the only token saving. This is the request path's tier.
func TestComputeSavings_BinaryFaithful_NoPhantomTokens(t *testing.T) {
	in := make([]byte, 4000) // 4000 binary bytes → len/4 = 1000 PHANTOM "tokens"
	md := "# Title\n\nshort recovered text"
	res := ApplyTier(Result{Markdown: md, Format: FormatDOCX}, TierFaithful)

	s := ComputeSavings(in, res)
	if s.TokensSaved != 0 {
		t.Errorf("binary at faithful must save 0 tokens (size reduction lives in bytes), not the len(bytes)/4 phantom; got %d", s.TokensSaved)
	}
	if s.InputTokensRaw != len(md)/4 {
		t.Errorf("binary raw baseline must be the faithful-text tokens (%d), NOT len(bytes)/4 (%d); got %d", len(md)/4, len(in)/4, s.InputTokensRaw)
	}
	if s.InputBytes != len(in) || s.OutputBytes != len(md) {
		t.Errorf("byte size-reduction must be carried: in=%d out=%d", s.InputBytes, s.OutputBytes)
	}
}

// Binary-origin at a REDUCING tier: the token saving is the tier delta vs the
// faithful-text baseline (a real saving), and STILL never the raw-bytes phantom.
func TestComputeSavings_BinaryReducingTier_TierDeltaOnly(t *testing.T) {
	in := make([]byte, 8000) // large binary; len/4 = 2000 phantom
	md := "# Title\n\nbody that outline drops\n\n## Section\n\nmore body that outline drops too"
	res := ApplyTier(Result{Markdown: md, Format: FormatDOCX}, TierOutline) // FaithfulTextTokens = faithful md

	s := ComputeSavings(in, res)
	faithfulTokens := len(md) / 4
	if s.InputTokensRaw != faithfulTokens {
		t.Errorf("raw baseline must be the faithful-text tokens %d; got %d", faithfulTokens, s.InputTokensRaw)
	}
	if s.TokensSaved <= 0 {
		t.Errorf("a reducing tier on binary should save the tier delta (>0); got %d", s.TokensSaved)
	}
	if s.TokensSaved != s.InputTokensRaw-s.InputTokensDistilled {
		t.Errorf("binary tier-delta saving must be faithful minus tiered tokens; got %d", s.TokensSaved)
	}
	if phantom := len(in)/4 - s.InputTokensDistilled; s.TokensSaved == phantom {
		t.Errorf("must NOT use the len(bytes)/4 phantom baseline (%d)", phantom)
	}
}

// FaithfulTextTokens must survive the cache JSON round-trip so a cache HIT on a
// binary-origin document computes the tier-delta savings correctly (not a 0/
// phantom). The ConverterVersion bump orphans pre-field ("1") entries; this pins
// that NEW ("2") entries carry the baseline through the cache.
func TestCache_FaithfulTextTokensSurvivesRoundTrip(t *testing.T) {
	res := Result{Format: FormatDOCX, Markdown: "# Title\n\nkept body", Tier: TierOutline, FaithfulTextTokens: 200}
	b, err := json.Marshal(cachedResult{Result: res})
	if err != nil {
		t.Fatal(err)
	}
	var cr cachedResult
	if err := json.Unmarshal(b, &cr); err != nil {
		t.Fatal(err)
	}
	if cr.Result.FaithfulTextTokens != 200 {
		t.Fatalf("FaithfulTextTokens lost across the cache round-trip: %d", cr.Result.FaithfulTextTokens)
	}
	// On the hit, the binary baseline is the cached FaithfulTextTokens (the tier
	// delta), never len(bytes)/4.
	s := computeSavings(make([]byte, 9999), cr.Result, true)
	if s.InputTokensRaw != 200 {
		t.Errorf("binary raw baseline on a cache hit must be the cached FaithfulTextTokens 200; got %d", s.InputTokensRaw)
	}
	if s.TokensSaved != 200-estTokens(res.Markdown) {
		t.Errorf("binary saving on a cache hit must be the tier delta (200 - distilled); got %d", s.TokensSaved)
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
