package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// hbKeyPrefix is the Redis namespace for per-node heartbeat records.
	// Key format: lens:cp:hb:{type}:{nodeID}   e.g. lens:cp:hb:inference:abc123
	hbKeyPrefix = "lens:cp:hb:"

	// hbTTL is 2× StaleThreshold. A node that misses two full stale-check cycles
	// has its Redis key expire, guaranteeing it is excluded from snapshots once
	// Postgres also marks it inactive. Mirrors the snapshotTTL safety margin.
	hbTTL = 2 * StaleThreshold // 3 minutes
)

// hbRecord is the JSON payload stored in Redis per node heartbeat.
type hbRecord struct {
	LastSeenAt    time.Time `json:"last_seen_at"`
	UptimeSeconds int64     `json:"uptime_seconds"`
}

// redisHB is the Redis subset HeartbeatStore needs. *redis.Client satisfies
// this interface; tests inject a miniredis-backed client through it.
type redisHB interface {
	Set(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.StatusCmd
	Get(ctx context.Context, key string) *redis.StringCmd
}

// HeartbeatStore buffers node heartbeats in Redis so that any Lens instance
// can record a heartbeat and the Reconciler — wherever it runs after a
// failover — can immediately read the freshest liveness state without waiting
// for a Postgres round-trip or a new node heartbeat cycle.
//
// This is the core xDS HA property: heartbeat data is "reused" across the
// cluster via a shared Redis key rather than being siloed per-instance in
// Postgres. When a leader election fires on instance B, B's Reconciler reads
// the same Redis heartbeats that instance A was maintaining, so the first
// snapshot it publishes reflects the current fleet rather than the last
// Postgres-persisted state.
//
// Liveness priority inside Snapshot():
//  1. Redis heartbeat fresh  → include (primary)
//  2. Redis absent/stale     → fall back to Postgres last_seen_at (secondary)
//  3. Both stale             → exclude
//
// A nil redis client is supported — Record becomes a safe no-op and IsFresh
// always returns (false, zero, nil), so callers transparently fall back to
// Postgres. Single-instance deployments without Redis are unaffected.
type HeartbeatStore struct {
	rdb redisHB
}

// NewHeartbeatStore returns a HeartbeatStore backed by rdb.
// Pass nil to disable — single-instance / no-Redis deployments are unchanged.
func NewHeartbeatStore(rdb *redis.Client) *HeartbeatStore {
	var db redisHB
	if rdb != nil {
		db = rdb
	}
	return &HeartbeatStore{rdb: db}
}

// newHeartbeatStore is the internal constructor used by tests (accepts the
// interface directly so a miniredis-backed *redis.Client can be injected
// without the public nil-pointer guard).
func newHeartbeatStore(rdb redisHB) *HeartbeatStore {
	return &HeartbeatStore{rdb: rdb}
}

func hbKey(nodeType, nodeID string) string {
	return hbKeyPrefix + nodeType + ":" + nodeID
}

// Record writes the heartbeat to Redis with hbTTL. nodeType must be one of
// "inference", "cache", or "embedding" — it is embedded in the key name.
// Safe no-op when rdb is nil.
func (h *HeartbeatStore) Record(ctx context.Context, nodeType, nodeID string, uptimeSeconds int64) error {
	if h.rdb == nil {
		return nil
	}
	rec := hbRecord{LastSeenAt: time.Now().UTC(), UptimeSeconds: uptimeSeconds}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("controlplane: marshal heartbeat: %w", err)
	}
	return h.rdb.Set(ctx, hbKey(nodeType, nodeID), data, hbTTL).Err()
}

// IsFresh reports whether the Redis heartbeat for nodeType/nodeID arrived
// within StaleThreshold. Returns (false, zero, nil) when no record exists in
// Redis — the caller should fall back to Postgres last_seen_at. An error is
// returned only for unexpected Redis failures, not for a missing key.
func (h *HeartbeatStore) IsFresh(ctx context.Context, nodeType, nodeID string) (bool, time.Time, error) {
	if h.rdb == nil {
		return false, time.Time{}, nil
	}
	data, err := h.rdb.Get(ctx, hbKey(nodeType, nodeID)).Bytes()
	if err != nil {
		if err == redis.Nil {
			return false, time.Time{}, nil // absent key — not an error
		}
		return false, time.Time{}, fmt.Errorf("controlplane: read heartbeat: %w", err)
	}
	var rec hbRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return false, time.Time{}, fmt.Errorf("controlplane: decode heartbeat: %w", err)
	}
	return time.Since(rec.LastSeenAt) < StaleThreshold, rec.LastSeenAt, nil
}
