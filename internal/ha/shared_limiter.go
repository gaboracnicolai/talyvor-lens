package ha

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const limiterKeyPrefix = "ha:rl:"

// sharedIncrScript atomically INCRs a fixed-window counter and sets its expiry
// on the first hit, returning the new count. Atomic via EVAL so concurrent
// instances cannot race between INCR and EXPIRE. This is the authoritative L2
// counter: because every instance INCRs the same key, the global count is
// exact and the (limit+1)-th request — from whichever instance — is the one
// that gets rejected. That is what bounds an N-instance deployment to the real
// cap instead of N×cap.
var sharedIncrScript = redis.NewScript(`
local count = redis.call('INCR', KEYS[1])
if count == 1 then
    redis.call('EXPIRE', KEYS[1], ARGV[1])
end
return count
`)

// Decision is the outcome of a SharedLimiter check.
type Decision struct {
	Allowed bool
	Count   int // count observed at the authoritative source (or local, if degraded)
	Limit   int
}

// SharedLimiter is a Redis-backed fixed-window rate limiter with an in-process
// L1 fast-reject in front of the authoritative L2 Redis counter.
//
//   - When enabled: L1 short-circuits the obvious case (this instance alone has
//     already spent the whole budget), otherwise L2 INCR is the global gate.
//   - When disabled (or constructed with a nil client): purely in-process — each
//     instance enforces the limit independently, exactly like a single-instance
//     deployment.
//   - When enabled but Redis is unreachable: degrades to the in-process counter
//     rather than failing fully open, so a Redis outage caps overshoot at
//     N×limit instead of unbounded.
type SharedLimiter struct {
	rdb     redisClient
	enabled bool
	local   *localWindow
}

// NewSharedLimiter builds a limiter. If enabled is true but rdb is nil it
// behaves as disabled (in-process only).
func NewSharedLimiter(rdb redisClient, enabled bool) *SharedLimiter {
	return &SharedLimiter{
		rdb:     rdb,
		enabled: enabled && rdb != nil,
		local:   newLocalWindow(),
	}
}

// Allow reports whether one request against `key` is permitted under `limit`
// per `window`. limit <= 0 means "no limit".
func (s *SharedLimiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (Decision, error) {
	if limit <= 0 {
		return Decision{Allowed: true, Limit: limit}, nil
	}

	if !s.enabled {
		n := s.local.add(key, window)
		return Decision{Allowed: n <= limit, Count: n, Limit: limit}, nil
	}

	// L1 fast reject: this instance has by itself already used the full budget,
	// so the global window is certainly exhausted — no need to touch Redis.
	if s.local.count(key, window) >= limit {
		return Decision{Allowed: false, Count: limit, Limit: limit}, nil
	}

	secs := int(window.Seconds())
	if secs < 1 {
		secs = 1
	}
	count, err := sharedIncrScript.Run(ctx, s.rdb, []string{limiterKeyPrefix + key}, secs).Int()
	if err != nil {
		slog.Warn("ha: shared limiter redis failed; degrading to in-process",
			slog.String("key", key), slog.String("err", err.Error()))
		n := s.local.add(key, window)
		return Decision{Allowed: n <= limit, Count: n, Limit: limit}, nil
	}

	allowed := count <= limit
	if allowed {
		// Keep L1 roughly in sync so the fast-reject path can fire later.
		s.local.add(key, window)
	}
	return Decision{Allowed: allowed, Count: count, Limit: limit}, nil
}

// localWindow is a tiny in-process fixed-window counter keyed by string. It is
// the L1 cache and the disabled/degraded fallback. Safe for concurrent use.
type localWindow struct {
	mu sync.Mutex
	m  map[string]*lwEntry
}

type lwEntry struct {
	count   int
	resetAt time.Time
}

func newLocalWindow() *localWindow { return &localWindow{m: map[string]*lwEntry{}} }

// entry returns the live window for key, rolling it over if it has expired.
// Caller must hold l.mu.
func (l *localWindow) entry(key string, window time.Duration, now time.Time) *lwEntry {
	e := l.m[key]
	if e == nil || now.After(e.resetAt) {
		e = &lwEntry{count: 0, resetAt: now.Add(window)}
		l.m[key] = e
	}
	return e
}

func (l *localWindow) count(key string, window time.Duration) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.entry(key, window, time.Now()).count
}

func (l *localWindow) add(key string, window time.Duration) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entry(key, window, time.Now())
	e.count++
	return e.count
}
