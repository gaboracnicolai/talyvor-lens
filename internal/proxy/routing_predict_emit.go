package proxy

import (
	"context"
	"errors"
	"log/slog"

	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/routingpredict"
)

// routingPredictEmitter is the WRITE-ONLY surface the live routing-prediction emit needs.
// *routingpredict.Store satisfies it. The emitter records an assertion ("cohort C → model M"); it mints
// NOTHING — routingpredict imports no ledger/mint symbol (pinned by its import-guard test). Minting happens
// far downstream (the offline scorer grades the prediction on the held eval slice; the routing-prediction
// minter pays rate × clamp01(skill_margin)), all still gated by the separate minting flags.
type routingPredictEmitter interface {
	EmitLivePrediction(ctx context.Context, p routingpredict.Prediction) (string, error)
}

// SetRoutingPredictEmit wires the live-emit surface + its capability flag (LENS_ROUTING_PREDICTION_ENABLED,
// read per-call). DEFAULT-OFF: with the flag off (the shipped default) the emitter is never called and the
// routing_predictions table stays provably empty from live traffic — the emit is the FIRST step of the
// held-routed → examined → mint chain, and turning it on is mint-free (recording a prediction credits
// nothing). nil emitter / nil-or-false flag ⇒ fully inert (byte-identical serve).
func (p *Proxy) SetRoutingPredictEmit(emitter routingPredictEmitter, enabled func() bool) {
	p.routingPredictEmitter = emitter
	p.routingPredictEmitEnabled = enabled
}

// emitRoutingPrediction records a routing PREDICTION for an auto-routed request whose cohort intelligence
// OVERRODE the baseline — POST-FLUSH, off-path, obsLimiter-shed, detached, best-effort, void. It runs after
// the response is flushed and CANNOT affect it.
//
// THE FARM GATE (cohortOverrode): only a decision where the cohort actually overrode the default becomes a
// prediction. A trivial request the base router already routes optimally — where the cohort did not
// override — emits nothing, so it can never reach the scorer or a mint. This mirrors the pattern-mining
// rarity floor: routine, non-improving decisions earn zero. The scorer's MinSliceSize warm-up and its
// clamp01(m_avg − baseline_avg) (a prediction that merely restates or underperforms the baseline scores 0)
// are the second, economic floor downstream.
//
// The emitted cohort is bound to cohort.DeriveInputCohort (the SAME derivation the serve path and the eval
// items use) so a prediction and its held-eval slice share a cohort key — otherwise the scorer would resolve
// the wrong slice, or none, and the prediction would never score (never mint, never strand).
func (p *Proxy) emitRoutingPrediction(ctx context.Context, workspaceID, feature, model, provider string,
	cohortOverrode bool, inputTokens int, complexityBucket string) {
	if p == nil || p.routingPredictEmitter == nil || p.routingPredictEmitEnabled == nil || !p.routingPredictEmitEnabled() {
		return
	}
	if !cohortOverrode {
		return // FARM GATE: the cohort did not override the baseline — a routine decision, not a prediction.
	}
	if model == "" || feature == "" || workspaceID == "" {
		return // incomplete cohort key — EmitLivePrediction would reject it; skip the write.
	}
	// Shed under overload, sharing the observational writer bound. A shed emit is harmless: predictions are
	// deduped one-live-per-cohort, so the cohort is (re)asserted by the next applicable request.
	if p.obsLimiter != nil {
		if !p.obsLimiter.TryAcquire() {
			if p.obsLimiter.LogDrop() {
				slog.Warn("routingpredict: live emit dropped (writer bound reached; serve unaffected)",
					slog.Int64("dropped_total", p.obsLimiter.Dropped()))
			}
			return
		}
		defer p.obsLimiter.Release()
	}
	// Detached + bounded (the K4 / routedecision capture discipline): survives request-ctx cancellation but
	// never outlives captureWriteTimeout.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), captureWriteTimeout)
	defer cancel()
	_, err := p.routingPredictEmitter.EmitLivePrediction(wctx, routingpredict.Prediction{
		WorkspaceID:      workspaceID,
		FeatureCategory:  feature,
		InputTokenRange:  mining.InputBucketFor(inputTokens), // canonical bucket; equals cohort.DeriveInputCohort's
		ComplexityBucket: complexityBucket,                   // the serve path's worktier bucket (same input)
		Model:            model,
		Provider:         provider,
	})
	if err != nil && !errors.Is(err, routingpredict.ErrDuplicatePrediction) {
		// ErrDuplicatePrediction is the EXPECTED steady state (a live prediction already holds the cohort
		// slot) — not logged. Any other error is best-effort-logged; the serve is already complete.
		slog.Warn("routingpredict: live emit failed (ignored; serve unaffected)", slog.String("err", err.Error()))
	}
}
