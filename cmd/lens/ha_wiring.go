package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/ha"
	"github.com/talyvor/lens/internal/metrics"
	"github.com/talyvor/lens/internal/ratelimit"
	"github.com/talyvor/lens/internal/retry"
)

// breakerStateValue maps a circuit-breaker state to the gauge encoding used by
// lens_circuit_breaker_state: 0 closed, 1 open, 2 half-open.
func breakerStateValue(s retry.CBState) float64 {
	switch s {
	case retry.CBOpen:
		return 1
	case retry.CBHalfOpen:
		return 2
	default:
		return 0
	}
}

// lensVersion is THE version string for this binary. Every surface that reports
// a version — /healthz, /status, the service page at /, the MCP server, the API
// server — must read it from here.
//
// It used to be duplicated as a bare "0.1.0" literal at five separate call
// sites, so bumping one would have left the others silently disagreeing about
// what was deployed. That is the same shape as a hardcoded money constant: right
// on the day it was written, quietly wrong afterwards, with nothing to catch it.
// TestVersionHasASingleSource keeps it that way.
//
// KNOWN LIMITATION, worth stating rather than implying otherwise: this is a
// source constant, not a build stamp. The release build passes only
// -ldflags="-w -s", so no version is injected and every deployment reports
// 0.1.0 regardless of what actually shipped. Wiring -X to this symbol (and to a
// git describe in CI) is the real fix; it touches the release pipeline and image
// build, so it is deliberately not bundled into a status-page change.
const lensVersion = "0.1.0"

// haComponents bundles the HA objects run() needs after setup: the health
// handlers to mount and the registry + breaker bus to drive graceful shutdown.
type haComponents struct {
	registry *ha.Registry
	health   *ha.Health
	breaker  *ha.SharedBreaker
	leader   *ha.Leader
}

// setupHA builds the High-Availability layer and wires it into the existing
// rate limiter and breaker registry. It is ALWAYS called; when cfg.HAEnabled is
// false every component degrades to a no-op / in-process fallback, so a
// single-instance deployment behaves exactly as it did before HA existed.
//
// The circuit breaker's state-change hook is installed here (rather than inline
// in run) so the structured-logging behaviour and the HA gossip publish compose
// in one place and work identically whether or not HA is enabled.
func setupHA(
	ctx context.Context,
	cfg *config.Config,
	rdb *redis.Client,
	pool *pgxpool.Pool,
	breakers *retry.BreakerRegistry,
	limiter *ratelimit.Limiter,
	logger *slog.Logger,
) *haComponents {
	hostname, _ := os.Hostname()
	self := ha.Instance{
		ID:        uuid.NewString(),
		Host:      hostname,
		StartedAt: time.Now(),
		Version:   lensVersion,
		Status:    ha.StatusActive,
	}

	registry := ha.NewRegistry(rdb, self, ha.RegistryConfig{
		Enabled:        cfg.HAEnabled,
		TTL:            cfg.HAInstanceTTL,
		HeartbeatEvery: cfg.HAHeartbeat,
	})
	registry.Start(ctx)

	// Shared limiter: authoritative cross-instance per-workspace cap. Attached
	// only under HA; otherwise the limiter is left byte-for-byte untouched. The
	// existing sliding-window limiter is itself already Redis-backed, so this is
	// an explicit global per-workspace cap on top of it, never a loosening.
	sharedLimiter := ha.NewSharedLimiter(rdb, cfg.HAEnabled)
	if cfg.HAEnabled {
		limiter.AttachShared(sharedRateAdapter{sl: sharedLimiter})
	}

	// Breaker gossip: publish local transitions and mirror peers' transitions.
	breaker := ha.NewSharedBreaker(rdb, breakers, self.ID, cfg.HAEnabled)
	breakers.SetOnStateChange(func(name string, from, to retry.CBState) {
		logger.Info("retry: circuit state change",
			slog.String("provider", name),
			slog.String("from", string(from)),
			slog.String("to", string(to)),
		)
		metrics.SetBreakerState(name, breakerStateValue(to)) // observability gauge
		breaker.Publish(name, to)                            // no-op when HA disabled
	})
	if err := breaker.Start(ctx); err != nil {
		logger.Warn("ha: breaker gossip subscribe failed", slog.String("err", err.Error()))
	}

	// Publish the active-instance count as a gauge while HA is enabled.
	if cfg.HAEnabled {
		go func() {
			t := time.NewTicker(cfg.HAHeartbeat)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if insts, err := registry.ActiveInstances(ctx); err == nil {
						metrics.SetHAInstanceCount(len(insts))
					}
				}
			}
		}()
	}

	// Readiness dependencies: the database always; Redis additionally when HA is
	// enabled (HA genuinely depends on Redis being reachable).
	deps := []ha.Dep{
		{Name: "database", Check: func(ctx context.Context) error {
			if pool == nil {
				return errors.New("no database pool")
			}
			return pool.Ping(ctx)
		}},
	}
	if cfg.HAEnabled {
		deps = append(deps, ha.Dep{Name: "redis", Check: func(ctx context.Context) error {
			return rdb.Ping(ctx).Err()
		}})
	}
	health := ha.NewHealth(registry, lensVersion, deps...)

	if cfg.HAEnabled {
		logger.Info("ha: high-availability mode enabled",
			slog.String("instance_id", self.ID),
			slog.String("host", self.Host),
			slog.Duration("heartbeat", cfg.HAHeartbeat),
			slog.Duration("instance_ttl", cfg.HAInstanceTTL),
			slog.Duration("drain_timeout", cfg.HADrainTimeout),
		)
	}

	leader := ha.NewLeader(rdb, self.ID, cfg.HAEnabled)

	return &haComponents{registry: registry, health: health, breaker: breaker, leader: leader}
}

// sharedRateAdapter adapts ha.SharedLimiter to ratelimit.SharedChecker, mapping
// a (workspace, key, rule) check to a per-workspace per-minute shared cap. A
// rule with no per-minute limit configured adds no cap.
type sharedRateAdapter struct{ sl *ha.SharedLimiter }

func (a sharedRateAdapter) Allow(ctx context.Context, wsID, _ string, rule ratelimit.RateRule) bool {
	limit := rule.RequestsPerMinute
	if limit <= 0 {
		return true
	}
	d, _ := a.sl.Allow(ctx, "ws:"+wsID, limit, time.Minute)
	return d.Allowed
}
