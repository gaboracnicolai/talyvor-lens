package proxy

import (
	"context"
	"log/slog"
	"math"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/routedecision"
)

// routeDecisionSink is the WRITE-ONLY persistence surface route-decision capture needs. *routedecision.Writer
// satisfies it. The sink CANNOT mint: routedecision imports no ledger and exposes only Exec/QueryRow.
type routeDecisionSink interface {
	Record(ctx context.Context, r routedecision.RouteDecision) error
}

// SetRouteDecision wires the descriptive route-decision sink + its enable flag (read per-call). main wires it
// default-ON (LENS_ROUTING_DECISION_CAPTURE_ENABLED) — it is descriptive, closed-test, and money-decoupled
// (the sink has no ledger handle). nil/off → no rows are written.
func (p *Proxy) SetRouteDecision(sink routeDecisionSink, enabled func() bool) {
	p.routeDecisionSink = sink
	p.routeDecisionEnabled = enabled
}

// routeTokens is the ONE token breakdown captureRouteDecision prices both models at. It exists so the two
// figures can never drift onto different counts (see the delta invariant below) and so the availability of a
// cache split travels with the numbers rather than being re-derived.
//
// ProviderReported says whether these came from the provider's usage report. False ⇒ there is no cache split
// to price: everything sits in UncachedInput and the row is labelled BasisFlat, because a flat figure that
// claims to be cache-aware is worse than one that admits what it is.
type routeTokens struct {
	UncachedInput    int
	CachedInput      int
	CacheWriteInput  int
	Output           int
	ProviderReported bool
}

// TotalInput is what the row stores as input_tokens — the tokens the request actually sent, unchanged in
// meaning for every existing reader of that column.
func (t routeTokens) TotalInput() int { return t.UncachedInput + t.CachedInput + t.CacheWriteInput }

// captureRouteDecision persists the routing Advisor's decision for an auto-routed request — POST-FLUSH,
// off-path, obsLimiter-shed, detached, best-effort, void, MINT-FREE. It runs after the response is flushed and
// CANNOT affect it. The counterfactual figure is an ESTIMATE (see routedecision / migration 0092), stored as
// evidence, never as money.
//
// ── THE COST BASIS ───────────────────────────────────────────────────────────
//
// Both models are priced with alerts.CostUSDDetailed, CACHE-AWARE: cached input at the provider's cache-read
// rate, cache writes at the write rate. This replaced alerts.CostUSD, which charged every input token at the
// full input rate. That old basis inflated the delta by exactly the cached-input discount the customer gets
// free from the provider — the naive-baseline error the README warns about, sitting inside the substrate meant
// to measure it. Correcting it makes the apparent saving SMALLER, which is the direction a correction to a
// savings claim should move.
//
// ⚠ TWO ASSUMPTIONS, both deliberately erring LOW on savings:
//
//  1. THE DELTA INVARIANT — both models are priced at the SAME token counts. counterfactual − actual is only
//     meaningful like-for-like; pricing the sides on different counts would corrupt every delta in the table.
//     So the served request's counts are used for both, and the only difference between the two figures is
//     the model's own rates.
//  2. THE BASELINE'S CACHE BEHAVIOUR IS ASSUMED EQUAL. The baseline call never happened, so it has no
//     reported cached-token counts. We apply the SAME split, priced at the baseline model's own cache rates.
//     Where the baseline would in fact have had no warm cache, this UNDER-states its cost and therefore
//     under-states the saving — the safe direction, and the same convention catalog.withCacheRates follows
//     ("deliberately conservative so we never over-claim savings"). Assuming no cache for the baseline would
//     have inflated the saving instead, so it is not an option.
func (p *Proxy) captureRouteDecision(ctx context.Context, workspaceID, baselineModel, actualModel, cohortBasis string,
	cohortOverrode bool, cohortN int, tk routeTokens) {
	if p == nil || p.routeDecisionSink == nil || p.routeDecisionEnabled == nil || !p.routeDecisionEnabled() {
		return
	}
	if baselineModel == "" || actualModel == "" {
		return // only auto-routed requests with a known baseline are evidence
	}
	// Shed under overload, sharing the observational writer bound.
	if p.obsLimiter != nil {
		if !p.obsLimiter.TryAcquire() {
			if p.obsLimiter.LogDrop() {
				slog.Warn("routedecision: observation dropped (writer bound reached; serve unaffected)",
					slog.Int64("dropped_total", p.obsLimiter.Dropped()))
			}
			return
		}
		defer p.obsLimiter.Release()
	}
	// Same counts, both sides — the delta invariant. CostUSDDetailed with zero cached/write tokens equals
	// CostUSD by construction (TestCostUSDDetailed_EqualsCostUSDAtZeroCached), so the no-provider-usage path
	// is byte-identical to the old behaviour rather than a second code path.
	actualU := usdToMicroUSD(alerts.CostUSDDetailed(actualModel,
		tk.UncachedInput, tk.CachedInput, tk.CacheWriteInput, tk.Output))
	counterfactualU := usdToMicroUSD(alerts.CostUSDDetailed(baselineModel,
		tk.UncachedInput, tk.CachedInput, tk.CacheWriteInput, tk.Output))
	basis := routedecision.BasisFlat
	if tk.ProviderReported {
		basis = routedecision.BasisCacheAware
	}

	// Detached + bounded, like the K4 verdict capture: the write survives request-ctx cancellation but never
	// outlives captureWriteTimeout.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), captureWriteTimeout)
	defer cancel()
	if err := p.routeDecisionSink.Record(wctx, routedecision.RouteDecision{
		WorkspaceID: workspaceID, BaselineModel: baselineModel, ActualModel: actualModel,
		CohortOverrode: cohortOverrode, CohortBasis: cohortBasis, CohortN: cohortN,
		InputTokens: tk.TotalInput(), OutputTokens: tk.Output,
		ActualCostU: actualU, CounterfactualCostEstimateU: counterfactualU,
		CostBasis: basis,
	}); err != nil {
		slog.Warn("routedecision: record failed (ignored; serve unaffected)", slog.String("err", err.Error()))
	}
}

// usdToMicroUSD converts a USD float to non-negative integer µ-USD (SEC-2 discipline: no float stored). A
// non-finite or non-positive cost → 0.
func usdToMicroUSD(usd float64) int64 {
	if usd <= 0 || math.IsNaN(usd) || math.IsInf(usd, 0) {
		return 0
	}
	return int64(math.Round(usd * 1e6))
}
