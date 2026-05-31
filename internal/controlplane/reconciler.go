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
//  2. Builds a NodeSnapshot of the still-live fleet and logs a summary.
//
// It is designed to be called under ha.Leader so exactly one Lens instance
// drives reconciliation in a multi-process deployment; the heartbeat-receiving
// HTTP endpoints run on every instance regardless.
type Reconciler struct {
	store *NodeStore
}

// NewReconciler builds a Reconciler backed by store.
func NewReconciler(store *NodeStore) *Reconciler {
	return &Reconciler{store: store}
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

	slog.Info("controlplane: reconcile",
		slog.Int("nodes_deactivated", deactivated),
		slog.Int("inference_live", len(snap.InferenceNodes)),
		slog.Int("cache_live", len(snap.CacheNodes)),
		slog.Int("embedding_live", len(snap.EmbeddingNodes)),
	)
}
