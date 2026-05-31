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
}

// NewNodeStore wraps a live connection pool.
func NewNodeStore(pool *pgxpool.Pool) *NodeStore {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newNodeStore(db)
}

func newNodeStore(pool pgxDB) *NodeStore {
	return &NodeStore{pool: pool}
}

// RecordEmbedHeartbeat refreshes last_seen_at and uptime_seconds for an
// embedding node. Mirrors the inference-node and cache-node heartbeat
// handlers in cmd/lens/main.go.
func (s *NodeStore) RecordEmbedHeartbeat(ctx context.Context, nodeID string, uptimeSeconds int64) error {
	if s.pool == nil {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE embedding_nodes
		SET last_seen_at = NOW(), uptime_seconds = $2
		WHERE id = $1`, nodeID, uptimeSeconds)
	return err
}

// MarkStaleInactive sets active=FALSE on inference_nodes, cache_nodes, and
// embedding_nodes for any row whose last heartbeat is older than threshold and
// is still marked active. Returns the total number of rows deactivated across
// all three tables.
func (s *NodeStore) MarkStaleInactive(ctx context.Context, threshold time.Duration) (int, error) {
	if s.pool == nil {
		return 0, nil
	}
	secs := int(threshold.Seconds())
	total := 0
	for _, table := range []string{"inference_nodes", "cache_nodes", "embedding_nodes"} {
		sql := fmt.Sprintf(`UPDATE %s SET active = FALSE
WHERE active = TRUE
  AND last_seen_at IS NOT NULL
  AND last_seen_at < NOW() - ($1 * INTERVAL '1 second')`, table)
		tag, err := s.pool.Exec(ctx, sql, secs)
		if err != nil {
			return total, fmt.Errorf("controlplane: mark stale %s: %w", table, err)
		}
		total += int(tag.RowsAffected())
	}
	return total, nil
}
