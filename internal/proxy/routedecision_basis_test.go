package proxy

import (
	"context"
	"testing"
)

// THE BASIS. routedecision priced BOTH models with alerts.CostUSD, which prices every input token at the
// full input rate — blind to prompt caching. Providers discount cached input heavily (Anthropic bills a
// cache read at 0.10x input), so every row captured that way measured the saving against a price the
// customer would never have paid. That is the same naive-baseline error the README now warns about, sitting
// inside the substrate meant to replace it.
//
// The models below are real catalog entries, so the arithmetic is the shipped arithmetic:
//
//	claude-sonnet-4-5 (baseline) $3.00/1M in, $15.00/1M out — anthropic ⇒ cache read 0.30, write 3.75
//	claude-haiku-4-5  (served)   $1.00/1M in,  $5.00/1M out — anthropic ⇒ cache read 0.10, write 1.25
//
// ⚠ THE HAIKU FIGURES CHANGED on 2026-07-26 and this is not a test adjustment — the CATALOG was wrong.
// It carried 0.80/4.00, which is HAIKU 3.5's rate, left behind when the id was updated to 4.5; the
// published rate is 1.00/5.00 (https://platform.claude.com/docs/en/about-claude/pricing). The constants
// below were RE-DERIVED from the published rates arithmetically, not replaced with whatever the code
// began emitting — the whole reason five wrong rates survived for months is tests that pinned the code's
// current output as if it were the truth. All four re-derived figures happened to match the new output
// exactly, which is a real cross-check rather than a coincidence to lean on.
//
// One request: 1M uncached input + 1M cached input + 1M output.
//
//	FLAT  (the old basis): actual 2.0×1.00 + 5.00 = $7.00 ; baseline 2.0×3.00 + 15.00 = $21.00 ; delta $14.00
//	CACHE-AWARE (correct): actual 1.00+0.10+5.00 = $6.10 ; baseline 3.00+0.30+15.00 = $18.30 ; delta $12.20
//
// The honesty property below is unaffected by the correction and still holds with room to spare:
// $12.20 cache-aware < $14.00 flat. Sonnet 4.5's rates were verified against the same page and are
// unchanged, so the baseline side of both figures is untouched.
const (
	basisBaseline = "claude-sonnet-4-5"
	basisActual   = "claude-haiku-4-5"

	wantActualCacheAwareU         = 6_100_000
	wantCounterfactualCacheAwareU = 18_300_000
	wantFlatActualU               = 7_000_000
	wantFlatCounterfactualU       = 21_000_000
)

func basisProxy(sink *fakeRouteSink) *Proxy {
	return &Proxy{routeDecisionSink: sink, routeDecisionEnabled: func() bool { return true }}
}

// Provider usage reported a cache breakdown ⇒ BOTH models are priced cache-aware.
func TestCaptureRouteDecision_PricesCacheAware(t *testing.T) {
	sink := &fakeRouteSink{}
	basisProxy(sink).captureRouteDecision(context.Background(), "ws1", basisBaseline, basisActual, "cohort", true, 5,
		routeTokens{UncachedInput: 1_000_000, CachedInput: 1_000_000, Output: 1_000_000, ProviderReported: true})

	if sink.calls != 1 {
		t.Fatalf("sink called %d times, want 1", sink.calls)
	}
	if got := sink.last.ActualCostU; got != wantActualCacheAwareU {
		t.Errorf("actual = %d µUSD, want %d (cache read at 0.10/1M, not the flat 1.00)", got, wantActualCacheAwareU)
	}
	if got := sink.last.CounterfactualCostEstimateU; got != wantCounterfactualCacheAwareU {
		t.Errorf("counterfactual = %d µUSD, want %d (baseline's OWN cache rate 0.30/1M)", got, wantCounterfactualCacheAwareU)
	}
}

// ⭐ THE HONESTY PROPERTY, and the reason this change matters: pricing the cache correctly makes the apparent
// saving SMALLER. The flat basis inflated the delta by charging the customer's free cache discount to the
// baseline. A fix that made the number bigger would be the wrong fix.
func TestCaptureRouteDecision_CacheAwareDeltaIsSmallerThanFlat(t *testing.T) {
	sink := &fakeRouteSink{}
	basisProxy(sink).captureRouteDecision(context.Background(), "ws1", basisBaseline, basisActual, "cohort", true, 5,
		routeTokens{UncachedInput: 1_000_000, CachedInput: 1_000_000, Output: 1_000_000, ProviderReported: true})

	cacheAware := sink.last.CounterfactualCostEstimateU - sink.last.ActualCostU
	flat := int64(wantFlatCounterfactualU - wantFlatActualU)
	if cacheAware >= flat {
		t.Errorf("cache-aware delta %d >= flat delta %d — the correction must reduce the claimed saving, never raise it",
			cacheAware, flat)
	}
	if cacheAware != wantCounterfactualCacheAwareU-wantActualCacheAwareU {
		t.Errorf("cache-aware delta = %d, want %d", cacheAware, wantCounterfactualCacheAwareU-wantActualCacheAwareU)
	}
}

// ⭐ THE DELTA INVARIANT: both models MUST be priced at the SAME token counts. counterfactual − actual is only
// meaningful as a like-for-like comparison; pricing the two sides on different counts would silently corrupt
// every delta in the table. Same counts, so the ONLY difference between the two figures is the model's rates.
func TestCaptureRouteDecision_BothSidesPricedAtSameTokens(t *testing.T) {
	sink := &fakeRouteSink{}
	// Identical models ⇒ identical cost, whatever the breakdown. If the two sides ever used different
	// counts, this equality would break.
	basisProxy(sink).captureRouteDecision(context.Background(), "ws1", basisActual, basisActual, "cohort", false, 0,
		routeTokens{UncachedInput: 700_000, CachedInput: 300_000, CacheWriteInput: 100_000, Output: 250_000, ProviderReported: true})

	if sink.last.ActualCostU != sink.last.CounterfactualCostEstimateU {
		t.Errorf("same model priced differently: actual %d vs counterfactual %d — the two sides are not on the same token counts",
			sink.last.ActualCostU, sink.last.CounterfactualCostEstimateU)
	}
	// And the cost is non-zero, so the equality above is a real comparison rather than 0 == 0.
	if sink.last.ActualCostU == 0 {
		t.Error("cost priced at 0 — the equality above would be vacuous")
	}
}

// No provider usage ⇒ no cache breakdown EXISTS, so the flat estimate is the only honest basis and the
// behaviour is byte-identical to before this change. The row is labelled so nobody mistakes it for measured.
func TestCaptureRouteDecision_NoProviderUsage_StaysFlatAndSaysSo(t *testing.T) {
	sink := &fakeRouteSink{}
	basisProxy(sink).captureRouteDecision(context.Background(), "ws1", basisBaseline, basisActual, "cohort", true, 5,
		routeTokens{UncachedInput: 2_000_000, Output: 1_000_000, ProviderReported: false})

	if got := sink.last.ActualCostU; got != wantFlatActualU {
		t.Errorf("actual = %d, want %d (flat: no breakdown exists to price)", got, wantFlatActualU)
	}
	if got := sink.last.CounterfactualCostEstimateU; got != wantFlatCounterfactualU {
		t.Errorf("counterfactual = %d, want %d (flat)", got, wantFlatCounterfactualU)
	}
	if sink.last.CostBasis != "flat" {
		t.Errorf("CostBasis = %q, want \"flat\" — an unlabelled row is indistinguishable from a measured one", sink.last.CostBasis)
	}
}

// The label is what stops a reader averaging across the changeover. Cache-aware rows say so.
func TestCaptureRouteDecision_LabelsTheBasis(t *testing.T) {
	sink := &fakeRouteSink{}
	basisProxy(sink).captureRouteDecision(context.Background(), "ws1", basisBaseline, basisActual, "cohort", true, 5,
		routeTokens{UncachedInput: 10, CachedInput: 10, Output: 10, ProviderReported: true})
	if sink.last.CostBasis != "cache_aware" {
		t.Errorf("CostBasis = %q, want \"cache_aware\"", sink.last.CostBasis)
	}
}

// The stored token counts stay the TOTAL input (uncached+cached+write) so input_tokens keeps meaning
// "tokens this request sent", unchanged for every existing reader of the column.
func TestCaptureRouteDecision_StoresTotalInputTokens(t *testing.T) {
	sink := &fakeRouteSink{}
	basisProxy(sink).captureRouteDecision(context.Background(), "ws1", basisBaseline, basisActual, "cohort", true, 5,
		routeTokens{UncachedInput: 700, CachedInput: 200, CacheWriteInput: 100, Output: 250, ProviderReported: true})
	if sink.last.InputTokens != 1000 {
		t.Errorf("InputTokens = %d, want 1000 (700+200+100)", sink.last.InputTokens)
	}
	if sink.last.OutputTokens != 250 {
		t.Errorf("OutputTokens = %d, want 250", sink.last.OutputTokens)
	}
}
