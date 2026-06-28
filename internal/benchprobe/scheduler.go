package benchprobe

import (
	"context"
	"fmt"
	"time"

	"github.com/talyvor/lens/internal/eval"
)

// Scheduler runs proof-of-benchmark probes off the hot path (mirrors the PoVI ChallengeScheduler
// rate-loop shape). Flag-gated: enabled() false ⇒ a total no-op (no draw, no delivery, no write).
// Delivery is injected (ProbeDelivery) — the live HTTPDelivery in prod, a fake in tests.
type Scheduler struct {
	store    *Store
	delivery ProbeDelivery
	enabled  func() bool
	nodes    NodeLister // optional: the active (node, model) targets for the periodic loop
}

// NodeTarget is one (node, model) the scheduler probes.
type NodeTarget struct {
	NodeID string
	Model  string
}

// NodeLister returns the active probe targets (e.g. active inference_nodes). Optional — only the
// StartScheduler/RunOnce loop needs it; RunOnceForNode is callable directly.
type NodeLister func(ctx context.Context) ([]NodeTarget, error)

// NewScheduler wires the store + delivery + the LENS_PROOF_OF_BENCHMARK_ENABLED gate.
func NewScheduler(store *Store, delivery ProbeDelivery, enabled func() bool) *Scheduler {
	return &Scheduler{store: store, delivery: delivery, enabled: enabled}
}

// SetNodeLister wires the active-target source for the periodic loop (RunOnce/StartScheduler).
func (s *Scheduler) SetNodeLister(l NodeLister) { s.nodes = l }

// RunOnceForNode draws ONE unpredictable, never-before-probed item for (nodeID, model), then:
//  1. HAPPENS-BEFORE: commits the benchmark_probes row (with request_id) BEFORE issuing the probe, so
//     the suppression key exists before the node's async receipt can arrive;
//  2. delivers the node-blind payload (input only) and scores the answer (eval.StaticScore);
//  3. fills in the probe + per-node score.
//
// No-op when the flag is off or the pool is exhausted for this node. MEASUREMENT only — touches no
// ledger and no mint.
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

	requestID := NewProbeRequestID()
	// (1) HAPPENS-BEFORE: commit the suppression key (request_id) before the probe is issued. pool.Exec
	// autocommits, so this row is durable before Deliver runs — a real receipt cannot precede it.
	if err := s.store.RecordProbe(ctx, Probe{NodeID: nodeID, ItemID: item.ID, RequestID: requestID, Score: 0}); err != nil {
		return err
	}

	// (2) deliver input ONLY — ground truth never leaves the verifier.
	req := BuildProbeRequest(model, *item)
	answer, err := s.delivery.Deliver(ctx, nodeID, requestID, req)
	if err != nil {
		return fmt.Errorf("benchprobe: deliver to %q: %w", nodeID, err)
	}
	score, _, serr := eval.StaticScore(eval.EvalMethod(item.EvalMethod), item.ExpectedOutput, answer)
	if serr != nil {
		return fmt.Errorf("benchprobe: score item %q: %w", item.ID, serr)
	}

	// (3) fill in the score on the committed probe row + fold the per-node average.
	if err := s.store.SetProbeScore(ctx, requestID, score); err != nil {
		return err
	}
	return s.store.UpsertNodeScore(ctx, nodeID, model, score)
}

// RunOnce probes one item for each active target. No-op when off or no lister is wired.
func (s *Scheduler) RunOnce(ctx context.Context) error {
	if s == nil || s.enabled == nil || !s.enabled() || s.nodes == nil {
		return nil
	}
	targets, err := s.nodes(ctx)
	if err != nil {
		return err
	}
	for _, tgt := range targets {
		if err := s.RunOnceForNode(ctx, tgt.NodeID, tgt.Model); err != nil {
			return err
		}
	}
	return nil
}

// StartScheduler runs RunOnce on a ticker until ctx is done (mirrors ChallengeScheduler).
func (s *Scheduler) StartScheduler(ctx context.Context, tick time.Duration) {
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
			_ = s.RunOnce(ctx)
		}
	}
}
