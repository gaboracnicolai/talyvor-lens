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

// captureRouteDecision persists the routing Advisor's decision for an auto-routed request — POST-FLUSH,
// off-path, obsLimiter-shed, detached, best-effort, void, MINT-FREE. It runs after the response is flushed and
// CANNOT affect it. It prices the served AND the counterfactual (baseline) model AT THE ACTUAL token counts —
// the counterfactual figure is an ESTIMATE (see routedecision / migration 0091), stored as evidence, never as
// money.
func (p *Proxy) captureRouteDecision(ctx context.Context, workspaceID, baselineModel, actualModel, cohortBasis string,
	cohortOverrode bool, cohortN, inputTokens, outputTokens int) {
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
	actualU := usdToMicroUSD(alerts.CostUSD(actualModel, inputTokens, outputTokens))
	counterfactualU := usdToMicroUSD(alerts.CostUSD(baselineModel, inputTokens, outputTokens))

	// Detached + bounded, like the K4 verdict capture: the write survives request-ctx cancellation but never
	// outlives captureWriteTimeout.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), captureWriteTimeout)
	defer cancel()
	if err := p.routeDecisionSink.Record(wctx, routedecision.RouteDecision{
		WorkspaceID: workspaceID, BaselineModel: baselineModel, ActualModel: actualModel,
		CohortOverrode: cohortOverrode, CohortBasis: cohortBasis, CohortN: cohortN,
		InputTokens: inputTokens, OutputTokens: outputTokens,
		ActualCostU: actualU, CounterfactualCostEstimateU: counterfactualU,
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
