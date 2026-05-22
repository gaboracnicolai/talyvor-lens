package ratelimit

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/redis/go-redis/v9"
)

type Limiter struct {
	redis *redis.Client
	mu    sync.RWMutex
	rules []RateRule
}

type RateRule struct {
	WorkspaceID       string
	KeyID             string
	RequestsPerSecond int
	RequestsPerMinute int
	RequestsPerHour   int
}

type RateLimitResult struct {
	Allowed        bool
	LimitType      string
	RetryAfterSecs int
	Remaining      int
}

func New(redisClient *redis.Client, rules []RateRule) *Limiter {
	cp := append([]RateRule(nil), rules...)
	return &Limiter{redis: redisClient, rules: cp}
}

// DefaultRules is the package-level entry point for sensible default
// limits. The Limiter method below delegates so callers can reach the
// defaults either via the package or via an instance.
func DefaultRules() []RateRule {
	return []RateRule{{
		RequestsPerSecond: 100,
		RequestsPerMinute: 1000,
		RequestsPerHour:   10000,
	}}
}

func (l *Limiter) DefaultRules() []RateRule {
	return DefaultRules()
}

func (l *Limiter) AddRule(rule RateRule) {
	l.mu.Lock()
	l.rules = append(l.rules, rule)
	l.mu.Unlock()
}

// matchRule scores each rule by specificity and returns the highest
// scorer that matches (wsID, keyID). Scoring:
//   - WorkspaceID + KeyID set and both match → 15
//   - WorkspaceID set and matches            → 10
//   - KeyID set and matches                  →  5
//   - Both empty (global)                    →  0
//
// A rule that names a workspace/key but doesn't match is skipped entirely.
// If no rule scores positive a zero RateRule is returned, meaning "no
// limits enforced".
func (l *Limiter) matchRule(wsID, keyID string) RateRule {
	l.mu.RLock()
	defer l.mu.RUnlock()

	bestScore := -1
	var best RateRule
	for _, r := range l.rules {
		score := 0
		if r.WorkspaceID != "" {
			if r.WorkspaceID != wsID {
				continue
			}
			score += 10
		}
		if r.KeyID != "" {
			if r.KeyID != keyID {
				continue
			}
			score += 5
		}
		if score > bestScore {
			bestScore = score
			best = r
		}
	}
	return best
}

// incrScript atomically INCRs a counter, sets its expiry on first hit,
// and returns {count, ttl}. Atomic via EVAL so we don't race between
// INCR and EXPIRE — that race could leave keys with no TTL and a
// permanently-tripped limiter.
var incrScript = redis.NewScript(`
local count = redis.call('INCR', KEYS[1])
if count == 1 then
    redis.call('EXPIRE', KEYS[1], ARGV[1])
end
return {count, redis.call('TTL', KEYS[1])}
`)

type window struct {
	name  string
	secs  int
	limit int
}

func windowsFor(rule RateRule) []window {
	return []window{
		{"second", 1, rule.RequestsPerSecond},
		{"minute", 60, rule.RequestsPerMinute},
		{"hour", 3600, rule.RequestsPerHour},
	}
}

// Check evaluates each window in turn. The first window that exceeds its
// limit short-circuits and returns Allowed=false with the matching
// LimitType. If every window passes, Remaining is the minimum across
// windows so the client sees the tightest budget.
//
// On Redis errors we fail open — better to overshoot a limit than block
// every request when Redis is briefly unavailable.
func (l *Limiter) Check(ctx context.Context, wsID, keyID string) RateLimitResult {
	rule := l.matchRule(wsID, keyID)

	if l.redis == nil {
		return RateLimitResult{Allowed: true}
	}

	minRemaining := -1
	for _, w := range windowsFor(rule) {
		if w.limit <= 0 {
			continue // no limit configured for this window
		}
		key := fmt.Sprintf("lens:rl:%s:%s:%s", wsID, keyID, w.name)
		raw, err := incrScript.Run(ctx, l.redis, []string{key}, w.secs).Slice()
		if err != nil {
			slog.Warn("ratelimit: redis script failed; failing open",
				slog.String("key", key),
				slog.String("err", err.Error()),
			)
			continue
		}
		if len(raw) < 2 {
			continue
		}
		count, _ := raw[0].(int64)
		ttl, _ := raw[1].(int64)

		if int(count) > w.limit {
			retry := int(ttl)
			if retry <= 0 {
				retry = w.secs
			}
			return RateLimitResult{
				Allowed:        false,
				LimitType:      w.name,
				RetryAfterSecs: retry,
				Remaining:      0,
			}
		}
		remaining := w.limit - int(count)
		if minRemaining < 0 || remaining < minRemaining {
			minRemaining = remaining
		}
	}
	if minRemaining < 0 {
		minRemaining = 0
	}
	return RateLimitResult{Allowed: true, Remaining: minRemaining}
}
