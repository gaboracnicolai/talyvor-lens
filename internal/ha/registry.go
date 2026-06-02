package ha

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Instance status values.
const (
	StatusActive   = "active"
	StatusDraining = "draining"
	StatusDead     = "dead"
)

const instanceKeyPrefix = "ha:instance:"

// Instance is one Lens process's entry in the shared registry.
type Instance struct {
	ID        string    `json:"id"`
	Host      string    `json:"host"`
	StartedAt time.Time `json:"started_at"`
	LastSeen  time.Time `json:"last_seen"`
	Version   string    `json:"version"`
	Status    string    `json:"status"` // active | draining | dead
}

// RegistryConfig configures a Registry. HeartbeatEvery defaults to TTL/3.
type RegistryConfig struct {
	Enabled        bool
	TTL            time.Duration
	HeartbeatEvery time.Duration
}

// Registry tracks the live Lens instances via Redis. Each instance SETs a
// TTL'd key and refreshes it on a heartbeat; a crashed instance's key simply
// expires. When disabled, every Redis-touching method is a safe no-op and
// ActiveInstances reports just this instance, so a single-instance deployment
// behaves exactly as before HA existed.
type Registry struct {
	rdb     redisClient
	ttl     time.Duration
	hb      time.Duration
	enabled bool

	mu   sync.RWMutex
	self Instance
}

// NewRegistry builds a registry for `self`. rdb may be nil when disabled.
func NewRegistry(rdb redisClient, self Instance, cfg RegistryConfig) *Registry {
	if self.Status == "" {
		self.Status = StatusActive
	}
	if self.StartedAt.IsZero() {
		self.StartedAt = time.Now()
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = 15 * time.Second
	}
	hb := cfg.HeartbeatEvery
	if hb <= 0 {
		hb = ttl / 3
	}
	return &Registry{rdb: rdb, ttl: ttl, hb: hb, enabled: cfg.Enabled, self: self}
}

// Enabled reports whether HA coordination is active.
func (r *Registry) Enabled() bool { return r.enabled }

// Self returns a snapshot of this instance's current registry entry.
func (r *Registry) Self() Instance {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.self
}

func (r *Registry) key(id string) string { return instanceKeyPrefix + id }

// Start fires an immediate heartbeat (so the instance is visible at once) then
// refreshes on a ticker until ctx is cancelled. No-op when disabled.
func (r *Registry) Start(ctx context.Context) {
	if !r.enabled {
		return
	}
	if err := r.Heartbeat(ctx); err != nil {
		slog.Warn("ha: initial heartbeat failed", slog.String("err", err.Error()))
	}
	go func() {
		t := time.NewTicker(r.hb)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := r.Heartbeat(ctx); err != nil {
					slog.Warn("ha: heartbeat failed", slog.String("err", err.Error()))
				}
			}
		}
	}()
}

// Heartbeat writes this instance's entry with a fresh LastSeen and the TTL.
// No-op when disabled.
func (r *Registry) Heartbeat(ctx context.Context) error {
	if !r.enabled {
		return nil
	}
	r.mu.Lock()
	r.self.LastSeen = time.Now()
	self := r.self
	r.mu.Unlock()
	return r.writeSelf(ctx, self)
}

func (r *Registry) writeSelf(ctx context.Context, self Instance) error {
	data, err := json.Marshal(self)
	if err != nil {
		return err
	}
	return r.rdb.Set(ctx, r.key(self.ID), data, r.ttl).Err()
}

// SetDraining marks this instance as draining. The local status flips
// regardless of HA mode — so /readyz reports 503 and a load balancer drains
// this instance even in a single-instance deployment — and the change is
// pushed to Redis immediately when HA is enabled so peers/LB see it fast.
func (r *Registry) SetDraining(ctx context.Context) error {
	r.mu.Lock()
	r.self.Status = StatusDraining
	r.self.LastSeen = time.Now()
	self := r.self
	r.mu.Unlock()
	if !r.enabled {
		return nil
	}
	return r.writeSelf(ctx, self)
}

// Deregister deletes this instance's key on graceful shutdown. No-op when
// disabled.
func (r *Registry) Deregister(ctx context.Context) error {
	if !r.enabled {
		return nil
	}
	r.mu.RLock()
	id := r.self.ID
	r.mu.RUnlock()
	return r.rdb.Del(ctx, r.key(id)).Err()
}

// Instances returns every instance currently present in Redis (any status).
// When disabled it returns just this instance.
func (r *Registry) Instances(ctx context.Context) ([]Instance, error) {
	if !r.enabled {
		return []Instance{r.Self()}, nil
	}
	return r.scan(ctx)
}

// ActiveInstances returns instances that are eligible to receive new work:
// status==active and whose Redis key has not yet expired. When disabled it
// returns just this instance.
func (r *Registry) ActiveInstances(ctx context.Context) ([]Instance, error) {
	if !r.enabled {
		return []Instance{r.Self()}, nil
	}
	all, err := r.scan(ctx)
	if err != nil {
		return nil, err
	}
	// Redis key TTL is the authoritative staleness gate: a crashed or silent
	// instance's key simply expires and scan() skips it (the SCAN→GET race is
	// already handled there). A clock-based LastSeen check would compare this
	// instance's local time against a timestamp written by a peer's clock,
	// which is unsound under NTP skew across hosts.
	out := make([]Instance, 0, len(all))
	for _, in := range all {
		if in.Status != StatusActive {
			continue
		}
		out = append(out, in)
	}
	return out, nil
}

// scan walks ha:instance:* and decodes each entry. Unreadable/!decodable
// entries are skipped rather than failing the whole scan.
func (r *Registry) scan(ctx context.Context) ([]Instance, error) {
	var (
		out    []Instance
		cursor uint64
	)
	for {
		keys, next, err := r.rdb.Scan(ctx, cursor, instanceKeyPrefix+"*", 100).Result()
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			data, err := r.rdb.Get(ctx, k).Bytes()
			if err != nil {
				if errors.Is(err, redis.Nil) {
					continue // expired between SCAN and GET
				}
				continue
			}
			var in Instance
			if json.Unmarshal(data, &in) == nil {
				out = append(out, in)
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return out, nil
}
