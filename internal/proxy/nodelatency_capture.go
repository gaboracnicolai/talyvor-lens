package proxy

import (
	"context"
	"log/slog"

	"github.com/talyvor/lens/internal/cohort"
	"github.com/talyvor/lens/internal/router"
)

// nodeLatencySink is the WRITE-ONLY persistence surface the node-latency capture needs — just RecordServe.
// *nodelatency.Store satisfies it. The sink CANNOT mint: nodelatency imports no ledger and exposes only an
// Exec surface (see internal/nodelatency + its import-guard test).
type nodeLatencySink interface {
	RecordServe(ctx context.Context, nodeID, featureCategory, inputTokenRange, complexityBucket, model string, latencyMs int64, costScore int) error
}

// SetNodeLatencySink wires the descriptive node-latency capture sink + its enable flag (read per-call).
// main wires it default-off (LENS_NODE_LATENCY_CAPTURE_ENABLED). nil/off → no capture runs, no rows written.
func (p *Proxy) SetNodeLatencySink(sink nodeLatencySink, enabled func() bool) {
	p.nodeLatencySink = sink
	p.nodeLatencyEnabled = enabled
}

// captureNodeLatency folds one gateway-measured node serve into the per-(node,cohort) latency aggregate —
// POST-SERVE (off the hot path), best-effort, void. It shares pattern/worktier capture's obsLimiter budget
// and detached-write discipline; a persist failure is logged and swallowed (the served response is already
// flushed and is never affected). DESCRIPTIVE + mint-free: the sink has no ledger handle.
//
// latencyMs is the GATEWAY-MEASURED node round-trip (time.Since around client.Do in tryNodeRouting) — the
// node cannot fake it without actually being fast. cohort + cost are derived here via the SAME pure
// input-functions the router/cohort machinery use (cohort.DeriveInputCohort + router.AnalyseComplexity),
// gateway-computed from the input the node was asked to serve — the node cannot influence them. model is the
// gateway-selected served model (aligns the aggregate to benchmark_node_scores' (node,model) grain for C).
func (p *Proxy) captureNodeLatency(nodeID, model, feature, prompt string, latencyMs int64) {
	if p == nil || p.nodeLatencySink == nil || p.nodeLatencyEnabled == nil || !p.nodeLatencyEnabled() {
		return
	}
	if nodeID == "" {
		return
	}
	// Shed under overload, sharing pattern/worktier capture's writer bound.
	if p.obsLimiter != nil {
		if !p.obsLimiter.TryAcquire() {
			if p.obsLimiter.LogDrop() {
				slog.Warn("nodelatency: observation dropped (writer bound reached; observational, serve unaffected)",
					slog.Int64("dropped_total", p.obsLimiter.Dropped()))
			}
			return
		}
		defer p.obsLimiter.Release()
	}
	inputTokenRange, complexityBucket := cohort.DeriveInputCohort(prompt)
	costScore := router.AnalyseComplexity(prompt).Score()

	wctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), captureWriteTimeout)
	defer cancel()
	if err := p.nodeLatencySink.RecordServe(wctx, nodeID, feature, inputTokenRange, complexityBucket, model, latencyMs, costScore); err != nil {
		slog.Warn("nodelatency: observation write failed (observational; serve unaffected)",
			slog.String("node", nodeID), slog.String("err", err.Error()))
	}
}
