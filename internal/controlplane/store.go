package controlplane

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// StaleThreshold is the heartbeat silence duration after which a node is
// considered dead and marked inactive. 90 seconds matches the comment in
// migration 0025 and gives nodes two missed 30-second heartbeat cycles before
// being evicted.
const StaleThreshold = 90 * time.Second

// pgxDB is the subset of *pgxpool.Pool the store needs.
type pgxDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// NodeStore handles all DB reads and writes for the node control plane.
// A nil pool is supported — all operations become safe no-ops, which keeps
// single-binary deployments without Postgres working unchanged.
type NodeStore struct {
	pool pgxDB
	// hb is optional. When non-nil, Snapshot() uses Redis heartbeat freshness
	// as the primary liveness signal instead of Postgres last_seen_at. This is
	// the xDS HA improvement: any instance's heartbeat write is immediately
	// visible to the Reconciler on any other instance after failover.
	hb *HeartbeatStore
}

// NewNodeStore wraps a live connection pool. Pass a non-nil hb to enable
// Redis-backed heartbeat liveness — the primary xDS HA improvement.
// Pass nil for hb to fall back to Postgres-only liveness (single-instance mode).
func NewNodeStore(pool *pgxpool.Pool, hb *HeartbeatStore) *NodeStore {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newNodeStore(db, hb)
}

// newNodeStore is the internal constructor used by tests.
func newNodeStore(pool pgxDB, hb *HeartbeatStore) *NodeStore {
	return &NodeStore{pool: pool, hb: hb}
}

// RecordEmbedHeartbeat refreshes last_seen_at and uptime_seconds for an
// embedding node. Mirrors the inference-node and cache-node heartbeat
// handlers in cmd/lens/main.go.
//
// workspaceID is required: the UPDATE is scoped to both id AND workspace_id
// so that an API key from workspace-A cannot keep alive workspace-B's nodes.
// Returns (true, nil) when a row was updated; (false, nil) when no matching
// row exists (node not found or workspace mismatch — callers should 404).
func (s *NodeStore) RecordEmbedHeartbeat(ctx context.Context, nodeID, workspaceID string, uptimeSeconds int64) (bool, error) {
	if s.pool == nil {
		return false, nil
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE embedding_nodes
		SET last_seen_at = NOW(), uptime_seconds = $2
		WHERE id = $1 AND workspace_id = $3`, nodeID, uptimeSeconds, workspaceID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// MarkStaleInactive sets active=FALSE on inference_nodes, cache_nodes, and
// embedding_nodes for any row whose last heartbeat is older than threshold and
// is still marked active. Returns the total number of rows deactivated across
// all three tables.
//
// Each table is queried with a literal, pre-written SQL statement — no
// fmt.Sprintf table interpolation — so the queries are stable and visible to
// static analysis tools.
func (s *NodeStore) MarkStaleInactive(ctx context.Context, threshold time.Duration) (int, error) {
	if s.pool == nil {
		return 0, nil
	}
	secs := int(threshold.Seconds())

	type staleQuery struct {
		table string
		sql   string
	}
	queries := []staleQuery{
		{
			table: "inference_nodes",
			sql: `UPDATE inference_nodes SET active = FALSE
WHERE active = TRUE
  AND last_seen_at IS NOT NULL
  AND last_seen_at < NOW() - ($1 * INTERVAL '1 second')`,
		},
		{
			table: "cache_nodes",
			sql: `UPDATE cache_nodes SET active = FALSE
WHERE active = TRUE
  AND last_seen_at IS NOT NULL
  AND last_seen_at < NOW() - ($1 * INTERVAL '1 second')`,
		},
		{
			table: "embedding_nodes",
			sql: `UPDATE embedding_nodes SET active = FALSE
WHERE active = TRUE
  AND last_seen_at IS NOT NULL
  AND last_seen_at < NOW() - ($1 * INTERVAL '1 second')`,
		},
	}

	total := 0
	for _, q := range queries {
		tag, err := s.pool.Exec(ctx, q.sql, secs)
		if err != nil {
			return total, fmt.Errorf("controlplane: mark stale %s: %w", q.table, err)
		}
		total += int(tag.RowsAffected())
	}
	return total, nil
}
