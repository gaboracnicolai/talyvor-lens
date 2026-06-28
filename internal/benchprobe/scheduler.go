package benchprobe

import (
	"context"
	"fmt"

	"github.com/talyvor/lens/internal/eval"
)

// Scheduler runs proof-of-benchmark probes off the hot path (mirrors the PoVI ChallengeScheduler
// rate-loop shape). Flag-gated: enabled() false ⇒ RunOnceForNode is a total no-op (no draw, no
// delivery, no write). Delivery is injected (ProbeDelivery) — faked in PR-A.
type Scheduler struct {
	store    *Store
	delivery ProbeDelivery
	enabled  func() bool
}

// NewScheduler wires the store + delivery + the LENS_PROOF_OF_BENCHMARK_ENABLED gate. A nil enabled
// (or one returning false) makes the scheduler inert.
func NewScheduler(store *Store, delivery ProbeDelivery, enabled func() bool) *Scheduler {
	return &Scheduler{store: store, delivery: delivery, enabled: enabled}
}

// RunOnceForNode draws ONE unpredictable, never-before-probed item for (nodeID, model), builds the
// node-blind payload (input only), delivers it, scores the answer against held ground truth via
// eval.StaticScore, and records the probe + folds the per-node score. No-op when the flag is off or
// the pool is exhausted for this node. MEASUREMENT only — touches no ledger and no mint.
func (s *Scheduler) RunOnceForNode(ctx context.Context, nodeID, model string) error {
	if s == nil || s.enabled == nil || !s.enabled() {
		return nil // flag off ⇒ byte-identical no-op
	}
	item, err := s.store.DrawItem(ctx, nodeID)
	if err != nil {
		return err
	}
	if item == nil {
		return nil // pool exhausted for this node
	}

	req := BuildProbeRequest(model, *item) // input ONLY — ground truth never leaves the verifier
	answer, err := s.delivery.Deliver(ctx, nodeID, req)
	if err != nil {
		return fmt.Errorf("benchprobe: deliver to %q: %w", nodeID, err)
	}

	// Score the returned text against the verifier-held expected output. Non-static methods
	// (heuristic/llm_judge) are out of scope for PR-A → unhandled ⇒ score 0 (recorded, low).
	score, _, serr := eval.StaticScore(eval.EvalMethod(item.EvalMethod), item.ExpectedOutput, answer)
	if serr != nil {
		return fmt.Errorf("benchprobe: score item %q: %w", item.ID, serr)
	}

	if err := s.store.RecordProbe(ctx, Probe{NodeID: nodeID, ItemID: item.ID, RequestID: "", Score: score}); err != nil {
		return err
	}
	return s.store.UpsertNodeScore(ctx, nodeID, model, score)
}
