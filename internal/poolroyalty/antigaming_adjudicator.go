// antigaming_adjudicator.go — Phase-2: the AUTO clawback trigger.
//
// This is the one deliberate wire from DETECTION to ACTION. Everything upstream
// is either read-only (RingDetector, DetectorReader) or human-driven
// (AdjudicationWriter.Adjudicate). The AutoAdjudicator runs the ring detector on
// a cadence DURING the holdback window and revokes the flagged held mints BEFORE
// they can settle — the moat.
//
// It COMPOSES, changes nothing: it calls the existing durable Adjudicate
// (record-before-revoke audit + the exactly-once Revoker: CAS held→revoked +
// RevokeHeldTx). So every safety property already proven for the human path
// holds here — idempotent, a revoked mint can never finalize, a finalized mint
// can never be revoked.
//
// TWO HARD GUARANTEES:
//   - DEFAULT OFF. enabled() gates the whole run; a nil/false gate makes RunOnce
//     a total no-op (no reads, no writes). A detector that wrongly revokes is
//     worse than none, so auto-revoke ships inert and is turned on deliberately.
//   - FAIL-CLOSED. If the detector errors, RunOnce aborts with NO clawback — it
//     never revokes on a partial/unknown picture. The held rows stay revocable
//     and are retried next tick; nothing is settled or burned on ambiguity.
package poolroyalty

import (
	"context"
	"log/slog"
	"time"

	"github.com/talyvor/lens/internal/metrics"
)

// ringDetectorSurface is the read-only detect seam (*RingDetector satisfies it).
type ringDetectorSurface interface {
	DetectSelfDealingRings(ctx context.Context, window time.Duration) ([]RingFlag, error)
}

// adjudicateSurface is the durable clawback seam (*AdjudicationWriter satisfies
// it): record-before-revoke + the exactly-once Revoker.
type adjudicateSurface interface {
	Adjudicate(ctx context.Context, d AdjudicationDecision) (string, RevokeReport, error)
}

// AutoAdjudicator wires ring detection to durable clawback. The zero/nil
// adjudicator is inert.
type AutoAdjudicator struct {
	detector   ringDetectorSurface
	adjudicate adjudicateSurface
	enabled    func() bool
	window     time.Duration
}

// NewAutoAdjudicator composes the detector and the durable clawback under an
// enable gate. A non-positive window defaults to 24h. A nil enable gate is
// treated as OFF (inert) — auto-revoke must be opted into.
func NewAutoAdjudicator(detector ringDetectorSurface, adjudicate adjudicateSurface, enabled func() bool, window time.Duration) *AutoAdjudicator {
	if window <= 0 {
		window = 24 * time.Hour
	}
	return &AutoAdjudicator{detector: detector, adjudicate: adjudicate, enabled: enabled, window: window}
}

// on reports whether auto-revoke is enabled (nil gate ⇒ off).
func (a *AutoAdjudicator) on() bool { return a != nil && a.enabled != nil && a.enabled() }

// RunOnce detects self-dealing rings and revokes the flagged held mints before
// settlement, returning the number revoked. Inert when disabled; fail-closed on
// a detector error (no clawback, retried next tick).
func (a *AutoAdjudicator) RunOnce(ctx context.Context) (int, error) {
	if !a.on() || a.detector == nil || a.adjudicate == nil {
		return 0, nil // DEFAULT OFF: total no-op
	}
	flags, err := a.detector.DetectSelfDealingRings(ctx, a.window)
	if err != nil {
		// FAIL-CLOSED: never revoke on an incomplete/ambiguous picture. The held
		// rows stay revocable; the next tick retries.
		return 0, err
	}
	if len(flags) == 0 {
		metrics.SetRoyaltyDetectorFlagged("antigaming", "ring_auto", 0)
		return 0, nil
	}
	// Distinct flagged request_ids → the revoke set. The evidence (component +
	// reason) is carried into the durable audit record via the resolution label.
	seen := map[string]struct{}{}
	ids := make([]string, 0, len(flags))
	for _, f := range flags {
		if _, dup := seen[f.RequestID]; dup {
			continue
		}
		seen[f.RequestID] = struct{}{}
		ids = append(ids, f.RequestID)
	}
	metrics.SetRoyaltyDetectorFlagged("antigaming", "ring_auto", float64(len(ids)))

	_, report, err := a.adjudicate.Adjudicate(ctx, AdjudicationDecision{
		FlagType:            "self_dealing_ring",
		ResolutionLabel:     "auto: transitive identity ring (contributor and requester one operator)",
		CandidateRequestIDs: ids,
		RevokeRequestIDs:    ids,
		DecidedBy:           "auto:antigaming-ring-detector",
	})
	if err != nil {
		return 0, err
	}
	revoked := report.Totals[OutcomeRevoked]
	if revoked > 0 {
		slog.Warn("antigaming: self-dealing ring clawed back before settlement",
			slog.Int("revoked", revoked), slog.Int("flagged", len(ids)))
	}
	return revoked, nil
}

// StartScheduler ticks RunOnce until ctx ends — mirrors the finalize/detector
// sweepers. Leader-elected by the caller. Inert while disabled.
func (a *AutoAdjudicator) StartScheduler(ctx context.Context, tick time.Duration) {
	if tick <= 0 {
		tick = time.Minute
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := a.RunOnce(ctx); err != nil {
				slog.Warn("antigaming: auto-adjudicate sweep failed (fail-closed; no clawback this tick)",
					slog.String("error", err.Error()))
			}
		}
	}
}
