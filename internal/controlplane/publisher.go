package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	snapshotKey = "lens:cp:snapshot"
	// snapshotTTL is 2× the default reconcile interval.  Two missed ticks in a
	// row must occur before the snapshot expires — a single slow tick won't
	// cause syncer misses.
	snapshotTTL = 2 * defaultReconcileInterval // 60 s
)

// redisPublisher is the Redis subset the publisher needs.  *redis.Client
// satisfies this interface unchanged; tests supply a miniredis-backed client.
type redisPublisher interface {
	Set(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.StatusCmd
	Get(ctx context.Context, key string) *redis.StringCmd
}

// Publisher serialises a NodeSnapshot to Redis after each reconcile tick so
// every Lens instance can read the latest live fleet without querying Postgres.
// A nil Redis client is supported — all operations become safe no-ops so
// single-instance deployments without Redis keep working unchanged.
type Publisher struct {
	rdb redisPublisher
}

// NewPublisher builds a Publisher backed by rdb. Pass nil to disable — methods
// become no-ops rather than panicking.
func NewPublisher(rdb *redis.Client) *Publisher {
	var db redisPublisher
	if rdb != nil {
		db = rdb
	}
	return &Publisher{rdb: db}
}

// newPublisher is the internal constructor used by tests (accepts the interface
// directly so a miniredis client can be passed without the nil-pointer guard).
func newPublisher(rdb redisPublisher) *Publisher {
	return &Publisher{rdb: rdb}
}

// Publish serialises snap to Redis with snapshotTTL.
func (p *Publisher) Publish(ctx context.Context, snap *NodeSnapshot) error {
	if p.rdb == nil {
		return nil
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("controlplane: marshal snapshot: %w", err)
	}
	return p.rdb.Set(ctx, snapshotKey, data, snapshotTTL).Err()
}

// Latest reads and deserialises the most recent snapshot from Redis.
// Returns (nil, nil) when no snapshot has been published yet — callers
// should treat nil as "no data yet" and skip their sync pass rather than
// returning an error.
func (p *Publisher) Latest(ctx context.Context) (*NodeSnapshot, error) {
	if p.rdb == nil {
		return nil, nil
	}
	data, err := p.rdb.Get(ctx, snapshotKey).Bytes()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, fmt.Errorf("controlplane: read snapshot: %w", err)
	}
	var snap NodeSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("controlplane: decode snapshot: %w", err)
	}
	return &snap, nil
}
