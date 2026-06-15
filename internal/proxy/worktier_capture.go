package proxy

import (
	"context"
	"log/slog"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/worktier"
)

// workTierSink is the WRITE-ONLY persistence surface WorkTier capture needs —
// just Record. *worktier.Store satisfies it. The sink CANNOT mint: worktier
// imports no ledger and exposes only Exec/Query (see internal/worktier).
type workTierSink interface {
	Record(ctx context.Context, workspaceID, feature, model, provider string, wt worktier.WorkTier,
		inputTokens, outputTokens int, costUSD float64, complexityScore int, piiDetected, guardrailFired bool) error
}

// SetWorkTier wires the descriptive classifier sink + its enable flag (read
// per-call). main wires it default-off (LENS_WORKTIER_ENABLED). nil/off → no
// classification runs and no rows are written.
func (p *Proxy) SetWorkTier(sink workTierSink, enabled func() bool) {
	p.workTierSink = sink
	p.workTierEnabled = enabled
}

// captureWorkTier classifies the served request and persists the observation —
// POST-FLUSH (off the hot path), best-effort, void. It shares pattern capture's
// obsLimiter budget and detached-write discipline; a classify/persist failure is
// logged and swallowed (the served response is already flushed and is never
// affected). DESCRIPTIVE + mint-free: the sink has no ledger handle.
//
// Complexity is derived here via router.AnalyseComplexity(prompt) — the SAME
// pure function on the SAME input the router analyzed (compressedPrompt,
// unchanged since compression), so the tier's complexity EQUALS the routing
// decision's by construction. It is recomputed here rather than threaded out of
// router.Route because Route is not called on all serve paths (cache hits,
// auto-route Recommend hits); a post-flush recompute on the identical input is
// deterministic-equal and avoids a sometimes-unset value across the routing
// branches. AnalyseComplexity is pure substring scans — negligible, and off the
// hot path regardless.
func (p *Proxy) captureWorkTier(ctx context.Context, workspaceID, feature, model, provider, prompt string,
	inputTokens, outputTokens int, piiDetected, guardrailFired bool, loggingPolicy string) {
	if p == nil || p.workTierSink == nil || p.workTierEnabled == nil || !p.workTierEnabled() {
		return
	}
	// Shed under overload, sharing pattern capture's writer bound.
	if p.obsLimiter != nil {
		if !p.obsLimiter.TryAcquire() {
			if p.obsLimiter.LogDrop() {
				slog.Warn("worktier: observation dropped (writer bound reached; observational, serve unaffected)",
					slog.Int64("dropped_total", p.obsLimiter.Dropped()))
			}
			return
		}
		defer p.obsLimiter.Release()
	}
	complexityScore := router.AnalyseComplexity(prompt).Score()
	costUSD := alerts.CostUSD(model, inputTokens, outputTokens)
	wt := worktier.Classify(inputTokens, outputTokens, costUSD, complexityScore, piiDetected, guardrailFired, loggingPolicy)

	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), captureWriteTimeout)
	defer cancel()
	if err := p.workTierSink.Record(wctx, workspaceID, feature, model, provider, wt,
		inputTokens, outputTokens, costUSD, complexityScore, piiDetected, guardrailFired); err != nil {
		slog.Warn("worktier: observation write failed (observational; serve unaffected)",
			slog.String("workspace", workspaceID), slog.String("err", err.Error()))
	}
}
