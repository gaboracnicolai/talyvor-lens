package api

import (
	"testing"

	"github.com/talyvor/lens/internal/metrics"
)

// distillSummary is the honest accounting: net = saved - vision cost, on the
// same len/4 token unit; cache hit rate is a distinct avoided-reconversion
// figure. These must hold exactly, including the negative-net window.
func TestDistillSummary_NetAndHitRate(t *testing.T) {
	got := distillSummary(metrics.DistillStats{
		TokensSaved: 1000, VisionTokensCost: 200, CacheHits: 3, CacheMisses: 1,
	})
	if got.TokensSaved != 1000 || got.VisionTokensCost != 200 {
		t.Errorf("passthrough wrong: saved=%v cost=%v", got.TokensSaved, got.VisionTokensCost)
	}
	if got.NetTokens != 800 {
		t.Errorf("net = %v, want 800 (saved - vision cost)", got.NetTokens)
	}
	if got.CacheLookups != 4 {
		t.Errorf("lookups = %v, want 4", got.CacheLookups)
	}
	if got.CacheHitRate != 0.75 {
		t.Errorf("hit rate = %v, want 0.75", got.CacheHitRate)
	}
}

// The honesty case: when vision OCR cost exceeds savings in a window, NET is
// shown NEGATIVE — never hidden, never floored to 0.
func TestDistillSummary_NegativeWindow(t *testing.T) {
	got := distillSummary(metrics.DistillStats{TokensSaved: 100, VisionTokensCost: 450})
	if got.NetTokens != -350 {
		t.Errorf("net = %v, want -350 (honest negative when OCR cost > savings)", got.NetTokens)
	}
}

// No lookups → hit rate is 0, not a divide-by-zero NaN.
func TestDistillSummary_ZeroLookups(t *testing.T) {
	got := distillSummary(metrics.DistillStats{TokensSaved: 50})
	if got.CacheLookups != 0 || got.CacheHitRate != 0 {
		t.Errorf("zero lookups: lookups=%v rate=%v, want 0/0", got.CacheLookups, got.CacheHitRate)
	}
}

// End-to-end: the /v1/api/distill/summary route returns JSON whose net is
// exactly saved - vision_cost (the honest wiring), regardless of absolute
// counter values accumulated by other tests in the process.
func TestAPI_DistillSummary_NetRelationship(t *testing.T) {
	metrics.DistillTokensSaved(500)
	metrics.DistillVisionTokensCost(120)
	metrics.DistillCache("hit", "conversion")
	metrics.DistillCache("miss", "conversion")

	s := newServer(serverDeps{version: "test"})
	r := newRouter(t, s)
	_, got := doJSON(t, r, "/v1/api/distill/summary")

	saved, _ := got["tokens_saved"].(float64)
	cost, _ := got["vision_tokens_cost"].(float64)
	net, _ := got["net_tokens"].(float64)
	if net != saved-cost {
		t.Errorf("net_tokens %v != tokens_saved-vision_tokens_cost (%v)", net, saved-cost)
	}
	if saved < 500 || cost < 120 {
		t.Errorf("counters not reflected: saved=%v cost=%v", saved, cost)
	}
	hits, _ := got["cache_hits"].(float64)
	misses, _ := got["cache_misses"].(float64)
	rate, _ := got["cache_hit_rate"].(float64)
	if lk := hits + misses; lk > 0 && rate != hits/lk {
		t.Errorf("hit rate %v != hits/(hits+misses) %v", rate, hits/lk)
	}
}
