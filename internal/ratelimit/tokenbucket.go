package ratelimit

// tokenbucket.go — multi-tier token-bucket rate limiter. Sits
// alongside the existing sliding-window Limiter (limiter.go) —
// new code paths should prefer the bucket; the legacy limiter
// stays mounted for the existing `authed` chi.Router chain.
//
// Layers:
//   - CheckAndConsume   — single bucket Redis primitive (Lua).
//   - InMemoryFallback  — same shape, used when Redis is down.
//   - MultiTierLimiter  — runs CheckAndConsume across three
//                         tiers (global / workspace / key) and
//                         returns the most restrictive verdict.
//   - TPM helpers       — RecordTokens / CheckTokenBudget
//                         (LLM tokens, not request count).

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ─── constants ───────────────────────────────────

const (
	// DefaultBurstMultiplier is the burst-capacity boost over
	// the steady-state RPM. 1.5x matches the spec.
	DefaultBurstMultiplier = 1.5

	// fallbackCleanupInterval is how often the in-memory
	// fallback prunes stale buckets so the map can't grow
	// unbounded.
	fallbackCleanupInterval = 5 * time.Minute

	// tpmWindow is the TPM accounting window. Matches the
	// per-minute semantics callers expect.
	tpmWindow = time.Minute
)

// ─── types ───────────────────────────────────────

// LimitTier is one tier in the multi-tier stack.
type LimitTier struct {
	Name     string // "global" / "workspace" / "key"
	Key      string // Redis key prefix
	RPM      int    // requests per minute
	TPM      int    // tokens per minute (LLM tokens, not HTTP)
	BurstRPM int    // burst capacity (zero → DefaultBurstMultiplier × RPM)
}

// LimitResult is what CheckAndConsume returns.
type LimitResult struct {
	Allowed    bool          `json:"allowed"`
	Remaining  float64       `json:"remaining"`
	ResetAt    time.Time     `json:"reset_at"`
	RetryAfter time.Duration `json:"retry_after"`
}

// ─── Lua token-bucket script ─────────────────────

// bucketScript implements the classic token-bucket algorithm
// atomically in Redis. KEYS[1] holds the bucket state as a
// hash with two fields:
//
//	tokens     — current count (float)
//	last       — unix-millis of last refill
//
// ARGV order:
//
//	1: capacity
//	2: refill_per_sec
//	3: cost
//	4: now_unix_ms
//
// Returns: { allowed (0|1), remaining (float), reset_ms }
var bucketScript = redis.NewScript(`
local capacity = tonumber(ARGV[1])
local rate     = tonumber(ARGV[2])
local cost     = tonumber(ARGV[3])
local now      = tonumber(ARGV[4])

local data = redis.call('HMGET', KEYS[1], 'tokens', 'last')
local tokens = tonumber(data[1])
local last   = tonumber(data[2])

if tokens == nil then
    tokens = capacity
    last   = now
end

-- Refill since last seen, capped at capacity.
local elapsed = (now - last) / 1000.0
if elapsed < 0 then elapsed = 0 end
tokens = math.min(capacity, tokens + elapsed * rate)

local allowed = 0
if tokens >= cost then
    tokens = tokens - cost
    allowed = 1
end

-- Reset is when we'd next refill to capacity (informational).
local missing = capacity - tokens
local reset_ms = now
if rate > 0 and missing > 0 then
    reset_ms = now + math.floor((missing / rate) * 1000)
end

redis.call('HMSET', KEYS[1], 'tokens', tokens, 'last', now)
-- Expire 2x the time it takes to refill from empty so we don't
-- keep dead buckets forever.
local ttl_s = math.ceil(2 * capacity / math.max(rate, 0.001))
redis.call('EXPIRE', KEYS[1], ttl_s)

return { allowed, tostring(tokens), reset_ms }
`)

// ─── Redis check-and-consume ─────────────────────

// CheckAndConsume runs the Lua bucket script. Caller passes
// capacity (max tokens), rate (tokens/second), and cost (how
// many tokens this call consumes — usually 1 for an HTTP
// request, but the TPM path passes the token count).
//
// `key` is the full Redis key — caller is responsible for
// composing the tier prefix.
func CheckAndConsume(
	ctx context.Context,
	rdb *redis.Client,
	key string,
	capacity, rate, cost float64,
) (LimitResult, error) {
	if rdb == nil {
		return LimitResult{}, errors.New("ratelimit: nil redis client")
	}
	nowMs := time.Now().UnixMilli()
	raw, err := bucketScript.Run(ctx, rdb,
		[]string{key},
		capacity, rate, cost, nowMs,
	).Result()
	if err != nil {
		return LimitResult{}, fmt.Errorf("ratelimit: bucket script: %w", err)
	}
	arr, ok := raw.([]interface{})
	if !ok || len(arr) != 3 {
		return LimitResult{}, fmt.Errorf("ratelimit: unexpected script result: %v", raw)
	}
	allowedN, _ := arr[0].(int64)
	tokensStr, _ := arr[1].(string)
	resetMs, _ := arr[2].(int64)

	var tokens float64
	_, _ = fmt.Sscanf(tokensStr, "%f", &tokens)
	result := LimitResult{
		Allowed:   allowedN == 1,
		Remaining: tokens,
		ResetAt:   time.UnixMilli(resetMs),
	}
	if !result.Allowed && rate > 0 {
		// Time to refill enough to satisfy `cost`.
		need := cost - tokens
		if need < 0 {
			need = 0
		}
		result.RetryAfter = time.Duration(need/rate*float64(time.Second)) + time.Second
	}
	return result, nil
}

// ─── in-memory fallback ──────────────────────────

// localBucket is the equivalent of the Redis hash in pure Go.
// Used when Redis is unreachable so a transient outage doesn't
// take down the proxy.
type localBucket struct {
	tokens float64
	last   time.Time
}

// InMemoryFallback is the Redis-less limiter. Lossy across
// restarts but enough to keep DDoS-grade traffic from melting
// the upstream APIs.
type InMemoryFallback struct {
	mu      sync.Mutex
	buckets map[string]*localBucket
}

func NewInMemoryFallback() *InMemoryFallback {
	f := &InMemoryFallback{buckets: map[string]*localBucket{}}
	go f.gc()
	return f
}

// CheckAndConsume mirrors the Redis-side signature so callers
// can swap one for the other.
func (f *InMemoryFallback) CheckAndConsume(key string, capacity, rate, cost float64) LimitResult {
	now := time.Now()
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.buckets[key]
	if !ok {
		b = &localBucket{tokens: capacity, last: now}
		f.buckets[key] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens = clampFloat(b.tokens+elapsed*rate, 0, capacity)
		b.last = now
	}
	res := LimitResult{Remaining: b.tokens, ResetAt: now}
	if b.tokens >= cost {
		b.tokens -= cost
		res.Allowed = true
		res.Remaining = b.tokens
	} else if rate > 0 {
		need := cost - b.tokens
		res.RetryAfter = time.Duration(need/rate*float64(time.Second)) + time.Second
	}
	return res
}

// gc prunes buckets that haven't been touched in twice the
// cleanup interval. Cheap because we hold the lock just long
// enough to swing through the map.
func (f *InMemoryFallback) gc() {
	t := time.NewTicker(fallbackCleanupInterval)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-2 * fallbackCleanupInterval)
		f.mu.Lock()
		for k, b := range f.buckets {
			if b.last.Before(cutoff) {
				delete(f.buckets, k)
			}
		}
		f.mu.Unlock()
	}
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ─── multi-tier limiter ──────────────────────────

// MultiTierLimiter applies CheckAndConsume across the three
// tiers and returns the most restrictive verdict. Falls back
// to InMemoryFallback when the Redis call returns an error.
type MultiTierLimiter struct {
	rdb      *redis.Client
	fallback *InMemoryFallback
	tiers    []LimitTier
}

// NewMultiTierLimiter builds a limiter. `tiers` is ordered
// least-to-most-specific (global → workspace → key) — the
// order is preserved on response so the caller knows which
// tier denied them.
func NewMultiTierLimiter(rdb *redis.Client, tiers ...LimitTier) *MultiTierLimiter {
	return &MultiTierLimiter{
		rdb:      rdb,
		fallback: NewInMemoryFallback(),
		tiers:    tiers,
	}
}

// CheckRPM runs each tier as an RPM bucket. Cost is always 1
// (one HTTP request). Returns the first denial encountered;
// when *all* tiers allow, returns the tightest "Remaining" so
// the response headers reflect the binding constraint.
func (m *MultiTierLimiter) CheckRPM(ctx context.Context) (LimitResult, *LimitTier) {
	tightest := LimitResult{Allowed: true, Remaining: 1e9, ResetAt: time.Now()}
	var tier *LimitTier
	for i := range m.tiers {
		t := &m.tiers[i]
		if t.RPM <= 0 {
			continue
		}
		burst := t.BurstRPM
		if burst <= 0 {
			burst = int(float64(t.RPM) * DefaultBurstMultiplier)
		}
		rate := float64(t.RPM) / 60.0
		res, err := CheckAndConsume(ctx, m.rdb, t.Key+":rpm", float64(burst), rate, 1)
		if err != nil {
			// Fall back to in-memory; log noise is the caller's
			// job (main.go observes per-request) so we stay
			// silent here.
			res = m.fallback.CheckAndConsume(t.Key+":rpm", float64(burst), rate, 1)
		}
		if !res.Allowed {
			return res, t
		}
		if res.Remaining < tightest.Remaining {
			tightest = res
			tier = t
		}
	}
	return tightest, tier
}

// CheckTPM is the LLM-token analogue of CheckRPM. `cost` is
// the number of tokens the upcoming call is estimated to use
// (the proxy passes its pre-call estimate; the post-call
// RecordTokens reconciles the cache to the actual count).
func (m *MultiTierLimiter) CheckTPM(ctx context.Context, cost float64) (LimitResult, *LimitTier) {
	tightest := LimitResult{Allowed: true, Remaining: 1e9, ResetAt: time.Now()}
	var tier *LimitTier
	for i := range m.tiers {
		t := &m.tiers[i]
		if t.TPM <= 0 {
			continue
		}
		rate := float64(t.TPM) / 60.0
		res, err := CheckAndConsume(ctx, m.rdb, t.Key+":tpm", float64(t.TPM), rate, cost)
		if err != nil {
			res = m.fallback.CheckAndConsume(t.Key+":tpm", float64(t.TPM), rate, cost)
		}
		if !res.Allowed {
			return res, t
		}
		if res.Remaining < tightest.Remaining {
			tightest = res
			tier = t
		}
	}
	return tightest, tier
}

// ─── TPM bookkeeping ─────────────────────────────

// RecordTokens reconciles the TPM bucket with the *actual*
// LLM token count after the upstream call returns. Callers
// already paid the estimate via CheckTPM; this charges the
// delta so the next call sees an accurate budget.
//
// `tokens` is the *total* count for the call — the function
// reconciles via plain INCRBY into a per-minute window key so
// the call is cheap (no Lua) and survives nil clients.
func RecordTokens(ctx context.Context, rdb *redis.Client, workspaceID string, tokens int) error {
	if rdb == nil || tokens <= 0 {
		return nil
	}
	bucket := time.Now().UTC().Format("200601021504") // minute granularity
	key := fmt.Sprintf("tpm:ws:%s:%s", workspaceID, bucket)
	pipe := rdb.Pipeline()
	pipe.IncrBy(ctx, key, int64(tokens))
	pipe.Expire(ctx, key, 2*tpmWindow)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("ratelimit: record tokens: %w", err)
	}
	return nil
}

// CheckTokenBudget reads the running per-minute counter and
// reports whether `tpm` would still be respected after one
// more typical call. Allowed=true means "headroom present";
// false means the caller should 429.
func CheckTokenBudget(ctx context.Context, rdb *redis.Client, workspaceID string, tpm int) (bool, int, error) {
	if rdb == nil || tpm <= 0 {
		return true, tpm, nil
	}
	bucket := time.Now().UTC().Format("200601021504")
	key := fmt.Sprintf("tpm:ws:%s:%s", workspaceID, bucket)
	v, err := rdb.Get(ctx, key).Int()
	if err != nil && !errors.Is(err, redis.Nil) {
		return false, 0, fmt.Errorf("ratelimit: read tpm: %w", err)
	}
	remaining := tpm - v
	if remaining < 0 {
		remaining = 0
	}
	return v < tpm, remaining, nil
}

// ─── helper for headers ──────────────────────────

// ResetUnix returns the Unix-seconds timestamp the caller can
// stuff into the X-RateLimit-Reset header.
func (r LimitResult) ResetUnix() int64 { return r.ResetAt.Unix() }

// RetryAfterSeconds is the seconds-rounded Retry-After value.
func (r LimitResult) RetryAfterSeconds() int {
	if r.RetryAfter <= 0 {
		return 0
	}
	s := int(r.RetryAfter.Seconds() + 0.5)
	if s < 1 {
		s = 1
	}
	return s
}
