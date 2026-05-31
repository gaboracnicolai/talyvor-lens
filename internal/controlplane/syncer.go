package controlplane

import (
	"context"
	"log/slog"
	"time"

	"github.com/talyvor/lens/internal/localrouter"
)

// endpointRegistry is the subset of *localrouter.Router the syncer uses.
// The interface keeps NodeSyncer testable without a real Router.
type endpointRegistry interface {
	Register(e *localrouter.LocalEndpoint)
	Remove(id string) bool
	List() []*localrouter.LocalEndpoint
}

// NodeSyncer reads the latest NodeSnapshot from Redis and reconciles it into
// a localrouter.Router so the proxy's smart endpoint selection (least-loaded,
// lowest-latency, round-robin) works across live mining nodes rather than only
// the statically configured Ollama/vLLM entries.
//
// The syncer runs on every Lens instance — not just the leader — because each
// instance manages its own local copy of the routing table.  The control-plane
// Reconciler (leader-only) is responsible for publishing; the syncer only reads.
type NodeSyncer struct {
	pub    *Publisher
	router endpointRegistry
}

// NewNodeSyncer builds a NodeSyncer.
func NewNodeSyncer(pub *Publisher, router *localrouter.Router) *NodeSyncer {
	return &NodeSyncer{pub: pub, router: router}
}

// Sync reads the latest snapshot from Redis and updates the local Router:
//   - Inference nodes in the snapshot are registered (or refreshed if already
//     present — Register is idempotent on ID).
//   - Inference nodes previously synced from mining that no longer appear in
//     the snapshot (stale / deactivated by the reconciler) are removed.
//
// Static endpoints (WorkspaceID == "") are never touched so config-driven
// Ollama/vLLM entries keep working regardless of snapshot content.
func (s *NodeSyncer) Sync(ctx context.Context) {
	snap, err := s.pub.Latest(ctx)
	if err != nil {
		slog.Warn("controlplane: syncer read snapshot failed", slog.String("err", err.Error()))
		return
	}
	if snap == nil {
		return // no snapshot published yet — nothing to sync
	}

	// Index live inference node IDs for O(1) lookup during the removal pass.
	live := make(map[string]struct{}, len(snap.InferenceNodes))
	for _, n := range snap.InferenceNodes {
		live[n.ID] = struct{}{}
		s.router.Register(&localrouter.LocalEndpoint{
			ID:            n.ID,
			WorkspaceID:   n.WorkspaceID,
			URL:           n.URL,
			Provider:      n.Provider,
			Models:        append([]string(nil), n.Models...),
			MaxConcurrent: n.MaxConcurrent,
			Active:        true,
		})
	}

	// Remove mining endpoints that dropped off the snapshot.
	for _, ep := range s.router.List() {
		if ep.WorkspaceID == "" {
			continue // static config endpoint — never remove
		}
		if _, ok := live[ep.ID]; !ok {
			s.router.Remove(ep.ID)
		}
	}

	slog.Debug("controlplane: syncer applied snapshot",
		slog.Int("inference_synced", len(snap.InferenceNodes)),
		slog.Time("snapshot_age", snap.GeneratedAt),
	)
}

// Run calls Sync on every tick until ctx is cancelled.
// Pass interval=0 to use the default (30 s).
func (s *NodeSyncer) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultReconcileInterval
	}
	s.Sync(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Sync(ctx)
		}
	}
}
