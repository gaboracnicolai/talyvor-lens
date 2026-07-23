package alerts

import (
	"math"
	"testing"
)

// TestCostUSDDetailed_CachedInputCostsFarLessThanUncached is the headline of the
// whole cost-basis fix. A response that reports 9,000 cache-READ input tokens and
// only 1,000 fresh (uncached) input tokens must cost DRAMATICALLY less than 10,000
// tokens all billed at the full input rate — because cache reads are ~0.1x on
// Anthropic. The assertion is on the DOLLAR figure, not a status code.
func TestCostUSDDetailed_CachedInputCostsFarLessThanUncached(t *testing.T) {
	const model = "claude-sonnet-4-5" // input $3.00/M; cache read $0.30/M (0.1x, verified live docs)

	cached := CostUSDDetailed(model, 1_000, 9_000, 0, 0) // 1k fresh + 9k cache-read, no output
	naive := CostUSD(model, 10_000, 0)                   // the OLD basis: all 10k at the full input rate

	// Exact: (1000*3.00 + 9000*0.30)/1e6 = (3000 + 2700)/1e6 = 0.0057
	if math.Abs(cached-0.0057) > 1e-9 {
		t.Fatalf("cache-aware cost = %v, want 0.0057", cached)
	}
	// Naive: 10000*3.00/1e6 = 0.03
	if math.Abs(naive-0.03) > 1e-9 {
		t.Fatalf("naive (old) cost = %v, want 0.03", naive)
	}
	// "Dramatically less": the cache-aware basis is well under a fifth of the naive one.
	if cached >= naive*0.5 {
		t.Fatalf("cache-aware cost %v must be far below half the naive cost %v", cached, naive)
	}
}

// TestCostUSDDetailed_ChargesCacheWriteAndReadAtOwnRates prices a full Anthropic
// breakdown: fresh input at the input rate, cache reads at 0.1x, cache creation
// (writes) at 1.25x, output at the output rate.
func TestCostUSDDetailed_ChargesCacheWriteAndReadAtOwnRates(t *testing.T) {
	// (1000*3.00 + 9000*0.30 + 248*3.75 + 500*15.00)/1e6
	// = (3000 + 2700 + 930 + 7500)/1e6 = 0.01413
	got := CostUSDDetailed("claude-sonnet-4-5", 1_000, 9_000, 248, 500)
	if math.Abs(got-0.01413) > 1e-9 {
		t.Fatalf("full cache-aware breakdown = %v, want 0.01413", got)
	}
}

// TestCostUSDDetailed_EqualsCostUSDAtZeroCached proves the additive path: the new
// detailed function collapses EXACTLY to the old CostUSD when there are no cached
// or cache-write tokens. This is why every existing CostUSD caller (the in-flight
// billing arc included) is unaffected until it opts into the breakdown.
func TestCostUSDDetailed_EqualsCostUSDAtZeroCached(t *testing.T) {
	models := []string{"gpt-4o", "gpt-4o-mini", "claude-opus-4-5", "claude-sonnet-4-5", "gemini-2.5-pro", "unknown-model"}
	ios := [][2]int{{0, 0}, {1_000, 500}, {1_000_000, 1_000_000}, {42, 7}}
	for _, m := range models {
		for _, io := range ios {
			in, out := io[0], io[1]
			old := CostUSD(m, in, out)
			neu := CostUSDDetailed(m, in, 0, 0, out)
			if math.Abs(old-neu) > 1e-12 {
				t.Errorf("CostUSDDetailed(%s,%d,0,0,%d)=%v != CostUSD=%v — additive path must collapse to old at zero cached",
					m, in, out, neu, old)
			}
		}
	}
}
