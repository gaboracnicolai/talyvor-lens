package ha

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/redis/go-redis/v9"

	"github.com/talyvor/lens/internal/retry"
)

const breakerChannel = "ha:breaker"

// breakerMsg is one gossiped breaker transition. Origin lets each instance
// ignore the echo of its own publishes.
type breakerMsg struct {
	Origin   string `json:"origin"`
	Provider string `json:"provider"`
	To       string `json:"to"`
}

// SharedBreaker is the cross-instance circuit-breaker gossip bus. Local breaker
// state stays authoritative for each instance; Redis pub/sub is only a
// propagation channel so a trip on one instance becomes visible on all the
// others. The flow is:
//
//	local transition  -> registry.onStateChange -> SharedBreaker.Publish
//	peer's publish     -> subscribe loop         -> registry.ApplyRemoteState
//
// ApplyRemoteState deliberately does NOT fire onStateChange, so a mirrored
// transition is not re-published — that, plus the Origin check, prevents echo
// storms. When disabled, Publish and Start are no-ops and each instance's
// breakers stay purely local.
type SharedBreaker struct {
	rdb      redisClient
	registry *retry.BreakerRegistry
	selfID   string
	enabled  bool

	mu  sync.Mutex
	sub *redis.PubSub
	ctx context.Context
}

// NewSharedBreaker builds a gossip bus. If enabled is true but rdb/registry is
// nil it degrades to disabled (local-only).
func NewSharedBreaker(rdb redisClient, registry *retry.BreakerRegistry, selfID string, enabled bool) *SharedBreaker {
	return &SharedBreaker{
		rdb:      rdb,
		registry: registry,
		selfID:   selfID,
		enabled:  enabled && rdb != nil && registry != nil,
	}
}

// Publish gossips a local breaker transition to peers. Safe to call from the
// registry's onStateChange hook (which runs on its own goroutine). No-op when
// disabled.
func (b *SharedBreaker) Publish(provider string, to retry.CBState) {
	if !b.enabled {
		return
	}
	ctx := b.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	data, err := json.Marshal(breakerMsg{Origin: b.selfID, Provider: provider, To: string(to)})
	if err != nil {
		return
	}
	if err := b.rdb.Publish(ctx, breakerChannel, data).Err(); err != nil {
		slog.Warn("ha: breaker gossip publish failed",
			slog.String("provider", provider), slog.String("err", err.Error()))
	}
}

// Start subscribes to the gossip channel and mirrors peer transitions into the
// local registry until ctx is cancelled. No-op when disabled.
func (b *SharedBreaker) Start(ctx context.Context) error {
	if !b.enabled {
		return nil
	}
	sub := b.rdb.Subscribe(ctx, breakerChannel)
	// Block until the subscription is confirmed so we don't miss early
	// publishes in tests / fast startup.
	if _, err := sub.Receive(ctx); err != nil {
		_ = sub.Close()
		return err
	}
	b.mu.Lock()
	b.sub = sub
	b.ctx = ctx
	b.mu.Unlock()

	ch := sub.Channel()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				b.apply(msg.Payload)
			}
		}
	}()
	return nil
}

// apply mirrors one gossiped transition, ignoring our own echoes.
func (b *SharedBreaker) apply(payload string) {
	var m breakerMsg
	if err := json.Unmarshal([]byte(payload), &m); err != nil {
		return
	}
	if m.Origin == b.selfID {
		return
	}
	b.registry.ApplyRemoteState(m.Provider, retry.CBState(m.To))
}

// Close tears down the subscription. Safe to call when disabled or never
// started.
func (b *SharedBreaker) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sub != nil {
		err := b.sub.Close()
		b.sub = nil
		return err
	}
	return nil
}
