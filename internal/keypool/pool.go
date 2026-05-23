// Package keypool implements a per-provider pool of API keys with
// round-robin selection, sliding-window error tracking, and an out-of-
// band health checker that can promote a previously-failing key back
// into rotation. Raw key material is held only in memory; the package
// is built so Stats() and the JSON API surface never expose secrets.
package keypool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

// windowSize is the sliding window over which a key's error rate is
// computed. 100 matches the spec; smaller pools naturally use a shorter
// effective window because windowLen never exceeds requestsSeen.
const windowSize = 100

// minWindowForUnhealth blocks the unhealthy-mark from firing on the
// very first error of a brand-new key. Without a floor, 1 error / 1
// total = 100% and an N-key pool collapses on its first request.
const minWindowForUnhealth = 1

// healthCheckInterval is the cadence at which StartHealthChecker probes
// quarantined keys. 5 minutes (per spec) is long enough that a flaky
// upstream gets time to settle but short enough that a transient outage
// doesn't leave keys benched forever.
const healthCheckInterval = 5 * time.Minute

var allowedProviders = map[string]struct{}{
	"openai":    {},
	"anthropic": {},
	"google":    {},
}

type PoolKey struct {
	ID        string
	Provider  string
	Key       string
	Alias     string
	RateLimit int

	mu           sync.Mutex
	RequestCount int64
	ErrorCount   int64
	LastUsedAt   time.Time
	LastErrorAt  *time.Time
	Healthy      bool

	window    [windowSize]bool // true = error
	windowLen int              // entries populated so far (caps at windowSize)
	windowIdx int              // next write position (mod windowSize)
}

type KeyStats struct {
	ID           string    `json:"id"`
	Provider     string    `json:"provider"`
	Alias        string    `json:"alias"`
	RequestCount int64     `json:"request_count"`
	ErrorCount   int64     `json:"error_count"`
	ErrorRate    float64   `json:"error_rate"`
	Healthy      bool      `json:"healthy"`
	LastUsedAt   time.Time `json:"last_used_at"`
}

type Pool struct {
	mu      sync.RWMutex
	keys    map[string][]*PoolKey
	counter map[string]int
}

func New() *Pool {
	return &Pool{
		keys:    make(map[string][]*PoolKey),
		counter: make(map[string]int),
	}
}

func (p *Pool) Add(provider, key, alias string, rateLimit int) (*PoolKey, error) {
	if _, ok := allowedProviders[provider]; !ok {
		return nil, fmt.Errorf("keypool: unsupported provider %q", provider)
	}
	if key == "" {
		return nil, errors.New("keypool: key required")
	}
	pk := &PoolKey{
		ID:        uuid.NewString(),
		Provider:  provider,
		Key:       key,
		Alias:     alias,
		RateLimit: rateLimit,
		Healthy:   true,
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys[provider] = append(p.keys[provider], pk)
	return pk, nil
}

// Get selects the next healthy key for provider via round-robin. The
// counter advances even when we skip unhealthy entries so a long-lived
// bad key doesn't pin selection to its neighbour.
func (p *Pool) Get(provider string) (*PoolKey, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pool := p.keys[provider]
	if len(pool) == 0 {
		return nil, fmt.Errorf("keypool: no keys configured for provider %q", provider)
	}
	start := p.counter[provider]
	for i := 0; i < len(pool); i++ {
		idx := (start + i) % len(pool)
		candidate := pool[idx]
		if isHealthy(candidate) {
			p.counter[provider] = (idx + 1) % len(pool)
			return candidate, nil
		}
	}
	return nil, fmt.Errorf("keypool: no healthy keys for provider %q", provider)
}

func isHealthy(k *PoolKey) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.Healthy
}

// Remove drops a key from the pool. Returns false if the key wasn't
// registered. Counter is left alone — Get's modulo handles re-sizing
// naturally on the next call.
func (p *Pool) Remove(keyID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for provider, list := range p.keys {
		for i, k := range list {
			if k.ID == keyID {
				p.keys[provider] = append(list[:i], list[i+1:]...)
				return true
			}
		}
	}
	return false
}

func (p *Pool) RecordSuccess(keyID string) {
	k := p.find(keyID)
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.RequestCount++
	k.LastUsedAt = time.Now().UTC()
	k.observeOutcomeLocked(false)
}

func (p *Pool) RecordError(keyID string) {
	k := p.find(keyID)
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.ErrorCount++
	now := time.Now().UTC()
	k.LastErrorAt = &now
	k.observeOutcomeLocked(true)
	if k.windowLen >= minWindowForUnhealth && k.windowErrorRateLocked() > 0.5 {
		k.Healthy = false
		slog.Warn("keypool: key marked unhealthy",
			slog.String("key_id", k.ID),
			slog.String("provider", k.Provider),
			slog.String("alias", k.Alias),
			slog.Float64("error_rate", k.windowErrorRateLocked()),
		)
	}
}

// observeOutcomeLocked writes the outcome into the circular window.
// Caller MUST hold k.mu — the public Record* methods do; the health
// helpers below take the lock themselves.
func (k *PoolKey) observeOutcomeLocked(isError bool) {
	k.window[k.windowIdx] = isError
	k.windowIdx = (k.windowIdx + 1) % windowSize
	if k.windowLen < windowSize {
		k.windowLen++
	}
}

func (k *PoolKey) windowErrorRateLocked() float64 {
	if k.windowLen == 0 {
		return 0
	}
	errs := 0
	for i := 0; i < k.windowLen; i++ {
		if k.window[i] {
			errs++
		}
	}
	return float64(errs) / float64(k.windowLen)
}

// MarkHealthy clears the error window and counters so a recovered key
// gets a clean slate. Without the counter reset, lifetime ErrorCount
// would stay high and confuse anyone reading Stats() later.
func (p *Pool) MarkHealthy(keyID string) {
	k := p.find(keyID)
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.ErrorCount = 0
	k.Healthy = true
	k.windowIdx = 0
	k.windowLen = 0
	for i := range k.window {
		k.window[i] = false
	}
}

func (p *Pool) find(keyID string) *PoolKey {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, list := range p.keys {
		for _, k := range list {
			if k.ID == keyID {
				return k
			}
		}
	}
	return nil
}

// Stats returns a snapshot of every key. The raw Key field is never
// included — callers that serialise this struct can be sure they're
// not leaking secrets. ErrorRate is the sliding-window rate, not the
// lifetime ratio, so a key that was bad and got better trends back to
// zero as fresh successes age out the errors.
func (p *Pool) Stats() []KeyStats {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []KeyStats
	for _, list := range p.keys {
		for _, k := range list {
			k.mu.Lock()
			out = append(out, KeyStats{
				ID:           k.ID,
				Provider:     k.Provider,
				Alias:        k.Alias,
				RequestCount: k.RequestCount,
				ErrorCount:   k.ErrorCount,
				ErrorRate:    k.windowErrorRateLocked(),
				Healthy:      k.Healthy,
				LastUsedAt:   k.LastUsedAt,
			})
			k.mu.Unlock()
		}
	}
	return out
}

// StartHealthChecker runs RunHealthCheckOnce on a 5-minute ticker. It
// exits when ctx is done and is safe to call as a goroutine.
func (p *Pool) StartHealthChecker(ctx context.Context, checkFn func(provider, key string) bool) {
	t := time.NewTicker(healthCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.RunHealthCheckOnce(ctx, checkFn)
		}
	}
}

// RunHealthCheckOnce iterates unhealthy keys and probes each with
// checkFn. Keys whose probe returns true are promoted back via
// MarkHealthy. Exposed so tests can drive a single cycle synchronously
// without waiting on a real-time ticker.
func (p *Pool) RunHealthCheckOnce(ctx context.Context, checkFn func(provider, key string) bool) {
	if checkFn == nil {
		return
	}
	// Snapshot the unhealthy keys under lock so the checkFn can take
	// arbitrarily long without holding the pool mutex.
	type probe struct {
		id       string
		provider string
		key      string
	}
	var probes []probe
	p.mu.RLock()
	for _, list := range p.keys {
		for _, k := range list {
			k.mu.Lock()
			h := k.Healthy
			k.mu.Unlock()
			if !h {
				probes = append(probes, probe{id: k.ID, provider: k.Provider, key: k.Key})
			}
		}
	}
	p.mu.RUnlock()

	for _, pr := range probes {
		if ctx.Err() != nil {
			return
		}
		if checkFn(pr.provider, pr.key) {
			p.MarkHealthy(pr.id)
		}
	}
}
