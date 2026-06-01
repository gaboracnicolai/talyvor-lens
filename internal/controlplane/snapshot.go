package controlplane

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// InferenceNodeEntry is a live compute-mining node usable for routing.
type InferenceNodeEntry struct {
	ID            string    `json:"id"`
	WorkspaceID   string    `json:"workspace_id"`
	URL           string    `json:"url"`
	Provider      string    `json:"provider"`
	Models        []string  `json:"models"`
	GPUType       string    `json:"gpu_type"`
	MaxConcurrent int       `json:"max_concurrent"`
	PricePerToken float64   `json:"price_per_token"`
	LastSeenAt    time.Time `json:"last_seen_at"`
	UptimeSeconds int64     `json:"uptime_seconds"`
}

// CacheNodeEntry is a live cache-mining node.
type CacheNodeEntry struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	URL         string    `json:"url"`
	MaxSizeGB   float64   `json:"max_size_gb"`
	LastSeenAt  time.Time `json:"last_seen_at"`
}

// EmbeddingNodeEntry is a live embedding-mining node.
type EmbeddingNodeEntry struct {
	ID            string    `json:"id"`
	WorkspaceID   string    `json:"workspace_id"`
	URL           string    `json:"url"`
	Model         string    `json:"model"`
	Dimensions    int       `json:"dimensions"`
	MaxBatch      int       `json:"max_batch"`
	SpeedTPS      int       `json:"speed_tps"`
	LastSeenAt    time.Time `json:"last_seen_at"`
	UptimeSeconds int64     `json:"uptime_seconds"`
}

// NodeSnapshot is the live routing table produced on each reconcile tick.
// It contains only nodes that are active and whose last heartbeat arrived
// within StaleThreshold.
type NodeSnapshot struct {
	InferenceNodes []InferenceNodeEntry `json:"inference_nodes"`
	CacheNodes     []CacheNodeEntry     `json:"cache_nodes"`
	EmbeddingNodes []EmbeddingNodeEntry `json:"embedding_nodes"`
	GeneratedAt    time.Time            `json:"generated_at"`
}

// Snapshot queries all three node tables for active nodes and returns a
// point-in-time view of the live fleet. Liveness is determined by isLive()
// which prefers Redis heartbeat freshness (primary) over Postgres last_seen_at
// (fallback). A nil pool returns an empty snapshot so callers don't need to
// nil-check.
func (s *NodeStore) Snapshot(ctx context.Context) (*NodeSnapshot, error) {
	snap := &NodeSnapshot{GeneratedAt: time.Now().UTC()}
	if s.pool == nil {
		return snap, nil
	}

	// The SQL no longer filters by last_seen_at — liveness filtering moves to
	// Go so that Redis heartbeats from any instance contribute to inclusion
	// rather than only the Postgres-persisted timestamp from whichever instance
	// last handled the HTTP heartbeat request.

	// ── Inference nodes ──────────────────────────────────────────────────────
	irows, err := s.pool.Query(ctx, `
		SELECT id, workspace_id, url, provider, models,
		       gpu_type, max_concurrent, price_per_token,
		       last_seen_at, uptime_seconds
		FROM inference_nodes
		WHERE active = TRUE
		ORDER BY last_seen_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("controlplane: snapshot inference: %w", err)
	}
	defer irows.Close()
	for irows.Next() {
		var n InferenceNodeEntry
		if err := irows.Scan(
			&n.ID, &n.WorkspaceID, &n.URL, &n.Provider, &n.Models,
			&n.GPUType, &n.MaxConcurrent, &n.PricePerToken,
			&n.LastSeenAt, &n.UptimeSeconds,
		); err != nil {
			continue // malformed row — skip, don't abort the whole snapshot
		}
		if s.isLive(ctx, "inference", n.ID, n.LastSeenAt) {
			snap.InferenceNodes = append(snap.InferenceNodes, n)
		}
	}
	if err := irows.Err(); err != nil {
		return nil, fmt.Errorf("controlplane: snapshot inference rows: %w", err)
	}

	// ── Cache nodes ──────────────────────────────────────────────────────────
	crows, err := s.pool.Query(ctx, `
		SELECT id, workspace_id, url, max_size_gb, last_seen_at
		FROM cache_nodes
		WHERE active = TRUE
		ORDER BY last_seen_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("controlplane: snapshot cache: %w", err)
	}
	defer crows.Close()
	for crows.Next() {
		var n CacheNodeEntry
		if err := crows.Scan(&n.ID, &n.WorkspaceID, &n.URL, &n.MaxSizeGB, &n.LastSeenAt); err != nil {
			continue
		}
		if s.isLive(ctx, "cache", n.ID, n.LastSeenAt) {
			snap.CacheNodes = append(snap.CacheNodes, n)
		}
	}
	if err := crows.Err(); err != nil {
		return nil, fmt.Errorf("controlplane: snapshot cache rows: %w", err)
	}

	// ── Embedding nodes ──────────────────────────────────────────────────────
	erows, err := s.pool.Query(ctx, `
		SELECT id, workspace_id, url, model, dimensions,
		       max_batch, speed_tps, last_seen_at, uptime_seconds
		FROM embedding_nodes
		WHERE active = TRUE
		ORDER BY last_seen_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("controlplane: snapshot embedding: %w", err)
	}
	defer erows.Close()
	for erows.Next() {
		var n EmbeddingNodeEntry
		if err := erows.Scan(
			&n.ID, &n.WorkspaceID, &n.URL, &n.Model, &n.Dimensions,
			&n.MaxBatch, &n.SpeedTPS, &n.LastSeenAt, &n.UptimeSeconds,
		); err != nil {
			continue
		}
		if s.isLive(ctx, "embedding", n.ID, n.LastSeenAt) {
			snap.EmbeddingNodes = append(snap.EmbeddingNodes, n)
		}
	}
	if err := erows.Err(); err != nil {
		return nil, fmt.Errorf("controlplane: snapshot embedding rows: %w", err)
	}

	return snap, nil
}

// isLive reports whether a node should appear in the current snapshot.
//
// Priority:
//  1. If a HeartbeatStore is configured and the Redis heartbeat is fresh
//     (arrived within StaleThreshold) → live. This covers the HA case where
//     a different Lens instance recorded the heartbeat.
//  2. If Redis is absent / not configured → fall back to Postgres last_seen_at.
//     This preserves behaviour for single-instance deployments without Redis.
//  3. Redis error → warn and fall back to Postgres (non-fatal; stale data is
//     better than no snapshot at all).
func (s *NodeStore) isLive(ctx context.Context, nodeType, nodeID string, pgLastSeen time.Time) bool {
	if s.hb != nil {
		fresh, _, err := s.hb.IsFresh(ctx, nodeType, nodeID)
		if err != nil {
			slog.Warn("controlplane: heartbeat liveness check failed",
				slog.String("type", nodeType),
				slog.String("id", nodeID),
				slog.String("err", err.Error()))
			// Redis error — fall through to Postgres check.
		} else if fresh {
			return true
		}
		// Redis says absent or stale — fall through to Postgres check.
	}
	return time.Since(pgLastSeen) < StaleThreshold
}
