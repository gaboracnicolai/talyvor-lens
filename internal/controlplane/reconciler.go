package controlplane

import (
	"context"
	"log/slog"
	"time"
)

const defaultReconcileInterval = 30 * time.Second

// Reconciler is a stateless control-plane loop. On each tick it:
//  1. Marks nodes whose last heartbeat is older than StaleThreshold inactive
//     in Postgres — the DB is the single source of truth for node health.
//  2. Builds a NodeSnapshot of the still-live fleet.
//  3. Publishes the snapshot to Redis so every Lens instance's NodeSyncer can
//     pull it and keep its local localrouter.Router current.
//
// It is designed to be called under ha.Leader so exactly one Lens instance
// drives reconciliation in a multi-process deployment; the heartbeat-receiving
// HTTP endpoints and the NodeSyncer run on every instance regardless.
type Reconciler struct {
	store *NodeStore
	pub   *Publisher
}

// NewReconciler builds a Reconciler backed by store and pub.
func NewReconciler(store *NodeStore, pub *Publisher) *Reconciler {
	return &Reconciler{store: store, pub: pub}
}

// Run starts the reconciliation loop and blocks until ctx is cancelled.
// Pass interval=0 to use the default (30 s).
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultReconcileInterval
	}
	// Reconcile immediately so the state is current before the first tick.
	r.reconcile(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.reconcile(ctx)
		}
	}
}

func (r *Reconciler) reconcile(ctx context.Context) {
	deactivated, err := r.store.MarkStaleInactive(ctx, StaleThreshold)
	if err != nil {
		slog.Warn("controlplane: mark stale failed", slog.String("err", err.Error()))
		// Continue to snapshot even on partial failure — partial data is better
		// than no data.
	}

	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		slog.Warn("controlplane: snapshot failed", slog.String("err", err.Error()))
		return
	}

	// Publish to Redis so every instance's NodeSyncer can pull the fresh fleet.
	if err := r.pub.Publish(ctx, snap); err != nil {
		slog.Warn("controlplane: publish snapshot failed", slog.String("err", err.Error()))
		// Non-fatal — DB state is still correct; syncer will retry next tick.
	}

	slog.Info("controlplane: reconcile",
		slog.Int("nodes_deactivated", deactivated),
		slog.Int("inference_live", len(snap.InferenceNodes)),
		slog.Int("cache_live", len(snap.CacheNodes)),
		slog.Int("embedding_live", len(snap.EmbeddingNodes)),
	)
}
