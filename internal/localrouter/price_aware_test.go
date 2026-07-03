package localrouter

import (
	"sync/atomic"
	"testing"
)

// F4-capstone step B proofs: price-aware selection with a gateway price cap. Pure in-memory Router (no DB) —
// the selector reads only the mu-guarded endpoint signals (price/quality/latency) set by the test.

func paEndpoint(r *Router, id, model string, priceDeclared float64, latencyMs int64, qScore float64, qSamples int) *LocalEndpoint {
	e := &LocalEndpoint{ID: id, Provider: "ollama", Models: []string{model}, Active: true, Healthy: true, AvgLatencyMs: latencyMs}
	r.Register(e)
	r.SetNodePrice(id, priceDeclared)
	r.UpdateQuality(id, model, qScore, qSamples)
	return e
}

// (proof 1) CLAMP — the load-bearing guard. effective_price = min(declared, ceiling); an unknown/≤0 price is
// treated AS the ceiling (a node can't declare 0 to look free). A node cannot improve OR worsen its rank by
// declaring an out-of-band price.
func TestPriceAware_Clamp(t *testing.T) {
	const ceiling = 0.50
	cases := []struct {
		declared, want float64
		note           string
	}{
		{0.60, 0.50, "above ceiling → clamped to ceiling"},
		{0.05, 0.05, "below ceiling → declared as-is"},
		{0.50, 0.50, "at ceiling → ceiling"},
		{0.0, 0.50, "zero/unknown → ceiling (cannot fake-free)"},
		{-1.0, 0.50, "negative → ceiling"},
	}
	for _, c := range cases {
		if got := effectivePrice(c.declared, ceiling); got != c.want {
			t.Errorf("effectivePrice(%v, %v) = %v, want %v (%s)", c.declared, ceiling, got, c.want, c.note)
		}
	}

	// Rank invariance: an over-ceiling node declaring 100.0 vs 0.50 selects the SAME — the out-of-band
	// declaration is clamped, so it can neither game nor tank its rank.
	for _, decl := range []float64{100.0, 0.50} {
		r := NewRouter(nil)
		r.SetPriceCeiling(func() float64 { return ceiling })
		paEndpoint(r, "over", "m", decl, 10, 0.9, 10)  // fast, quality-passing, over/at-ceiling price
		paEndpoint(r, "cheap", "m", 0.05, 10, 0.9, 10) // cheaper, same latency+quality
		got, err := r.SelectEndpoint("m", StrategyPriceAware)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != "cheap" {
			t.Errorf("declared=%v: cheap (0.05) must beat over (clamped to %v); got %s", decl, ceiling, got.ID)
		}
	}
}

// (proof 2) QUALITY GATE — a cheap node that FAILS the held-probe C-gate (score<0.7 OR samples<warmup) is
// INELIGIBLE, even though cheapest.
func TestPriceAware_QualityGateExcludesCheapButUnqualified(t *testing.T) {
	// (a) below the score threshold.
	r := NewRouter(nil)
	r.SetPriceCeiling(func() float64 { return 0.50 })
	paEndpoint(r, "cheap-bad", "m", 0.01, 10, 0.50, 20) // cheapest but score 0.50 < 0.70
	paEndpoint(r, "ok", "m", 0.05, 10, 0.90, 20)
	got, err := r.SelectEndpoint("m", StrategyPriceAware)
	if err != nil || got.ID != "ok" {
		t.Fatalf("below-threshold cheap node must be excluded; got %v err=%v", got, err)
	}

	// (b) insufficient samples (warmup).
	r2 := NewRouter(nil)
	r2.SetPriceCeiling(func() float64 { return 0.50 })
	paEndpoint(r2, "cheap-cold", "m", 0.01, 10, 0.99, 2) // cheapest, high score, but only 2 samples (<5)
	paEndpoint(r2, "ok", "m", 0.05, 10, 0.90, 20)
	got2, err := r2.SelectEndpoint("m", StrategyPriceAware)
	if err != nil || got2.ID != "ok" {
		t.Fatalf("insufficient-samples cheap node must be excluded; got %v err=%v", got2, err)
	}
}

// (proof 3) COMPOSITE — among quality-passing nodes the selector ranks by the price/latency composite, NOT
// pure-cheapest. The cheapest node is slower; a slightly-pricier faster node wins.
func TestPriceAware_CompositeNotPureCheapest(t *testing.T) {
	r := NewRouter(nil)
	r.SetPriceCeiling(func() float64 { return 0.50 })
	// cheap-slow: cost = 0.05 × (1 + 300/100) = 0.20
	paEndpoint(r, "cheap-slow", "m", 0.05, 300, 0.9, 10)
	// pricier-fast: cost = 0.08 × (1 + 20/100) = 0.096  → wins
	paEndpoint(r, "pricier-fast", "m", 0.08, 20, 0.9, 10)
	got, err := r.SelectEndpoint("m", StrategyPriceAware)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "pricier-fast" {
		t.Errorf("composite (price×latency) must win, not raw-cheapest: got %s, want pricier-fast", got.ID)
	}
}

// (proof 4) INERT — when StrategyPriceAware is NOT selected, SelectEndpoint is unchanged. Price is ignored on
// the default path (least-loaded still decides), so no silent reroute.
func TestPriceAware_InertOnDefaultPath(t *testing.T) {
	r := NewRouter(nil)
	r.SetPriceCeiling(func() float64 { return 0.50 })
	// "expensive" is idle (activeCount 0); "cheap" is busy. StrategyLeastLoaded MUST pick expensive-but-idle
	// (price ignored) — proving the price signal does not leak into the existing strategy.
	paEndpoint(r, "expensive-idle", "m", 0.40, 10, 0.9, 10)
	busy := paEndpoint(r, "cheap-busy", "m", 0.01, 10, 0.9, 10)
	atomic.StoreInt64(&busy.activeCount, 5) // in-flight; least-loaded should avoid it
	got, err := r.SelectEndpoint("m", StrategyLeastLoaded)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "expensive-idle" {
		t.Errorf("StrategyLeastLoaded must ignore price and pick the idle node; got %s", got.ID)
	}
}

// (proof 1b) SELECTION-LEVEL unsynced-0 clamp — the clamp holds THROUGH selectPriceAware, not just in the
// effectivePrice unit. Catches a future fast-path refactor that reads a raw 0 price as "cheapest". An
// unsynced node (pricePerToken never set — the just-registered zero-value) is scored at the CEILING, so a
// genuine cheap node beats it; and a LONE unsynced node is still selectable (served at the ceiling).
func TestPriceAware_UnsyncedZeroClampedInSelection(t *testing.T) {
	// (a) unsynced-0 loses to a genuine cheap node (same latency + quality). If the 0 read as "free" it
	// would have the lowest cost and wrongly win.
	r := NewRouter(nil)
	r.SetPriceCeiling(func() float64 { return 0.50 })
	// "unsynced": Register + quality only — NO SetNodePrice, so pricePerToken stays the zero-value (never synced).
	r.Register(&LocalEndpoint{ID: "unsynced", Provider: "ollama", Models: []string{"m"}, Active: true, Healthy: true, AvgLatencyMs: 10})
	r.UpdateQuality("unsynced", "m", 0.9, 10)
	paEndpoint(r, "real", "m", 0.05, 10, 0.9, 10) // genuine cheap node
	got, err := r.SelectEndpoint("m", StrategyPriceAware)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "real" {
		t.Fatalf("unsynced-0 node must be scored at the ceiling (0.50) and LOSE to real (0.05); got %s — clamp bypassed at selection", got.ID)
	}

	// (b) a LONE unsynced-0 node is still selectable (scored at ceiling, but the only option) — a node
	// pending its first price sync must still serve, just charged at the ceiling. Must NOT error.
	r2 := NewRouter(nil)
	r2.SetPriceCeiling(func() float64 { return 0.50 })
	r2.Register(&LocalEndpoint{ID: "lonely", Provider: "ollama", Models: []string{"m"}, Active: true, Healthy: true, AvgLatencyMs: 10})
	r2.UpdateQuality("lonely", "m", 0.9, 10)
	got2, err := r2.SelectEndpoint("m", StrategyPriceAware)
	if err != nil {
		t.Fatalf("a lone unsynced node must still be selectable (served at the ceiling), got err=%v", err)
	}
	if got2.ID != "lonely" {
		t.Fatalf("lone unsynced node must be returned; got %v", got2)
	}
}
