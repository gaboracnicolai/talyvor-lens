package ha

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// The core correctness claim of the whole HA limiter: no matter how many
// instances share one Redis bucket, they cannot collectively allow more than
// the cap. This is what prevents an N-instance deployment from letting a
// workspace exceed its real limit by Nx.
func TestSharedLimiter_ConcurrentInstancesCannotExceedCap(t *testing.T) {
	_, mr := newTestRedis(t)

	const (
		instances       = 8
		limit           = 20
		attemptsPerInst = 100
		window          = time.Minute
		key             = "ws:acme:rpm"
	)

	// Each "instance" gets its own client to the SAME miniredis — exactly how
	// N Lens pods would each hold their own connection to one shared Redis.
	limiters := make([]*SharedLimiter, instances)
	for i := range limiters {
		rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = rc.Close() })
		limiters[i] = NewSharedLimiter(rc, true)
	}

	var allowed int64
	var wg sync.WaitGroup
	for i := 0; i < instances; i++ {
		wg.Add(1)
		go func(l *SharedLimiter) {
			defer wg.Done()
			ctx := context.Background()
			for j := 0; j < attemptsPerInst; j++ {
				d, err := l.Allow(ctx, key, limit, window)
				if err != nil {
					t.Errorf("Allow: %v", err)
					return
				}
				if d.Allowed {
					atomic.AddInt64(&allowed, 1)
				}
			}
		}(limiters[i])
	}
	wg.Wait()

	if allowed > limit {
		t.Fatalf("allowed %d requests across %d instances; cap is %d — instances exceeded the shared cap",
			allowed, instances, limit)
	}
	if allowed != limit {
		t.Fatalf("allowed %d, want exactly %d (enough attempts to saturate the bucket)", allowed, limit)
	}
}

func TestSharedLimiter_DisabledFallsBackToInProcess(t *testing.T) {
	// Disabled, nil client: must work entirely in-process.
	s := NewSharedLimiter(nil, false)
	ctx := context.Background()
	const limit = 3

	allowed := 0
	for i := 0; i < 5; i++ {
		d, err := s.Allow(ctx, "k", limit, time.Minute)
		if err != nil {
			t.Fatalf("Allow: %v", err)
		}
		if d.Allowed {
			allowed++
		}
	}
	if allowed != limit {
		t.Fatalf("disabled limiter allowed %d, want %d (in-process enforcement)", allowed, limit)
	}
}

func TestSharedLimiter_DisabledInstancesDoNotShare(t *testing.T) {
	// Two disabled limiters are independent — proving disabled mode is purely
	// in-process with no cross-instance coordination.
	a := NewSharedLimiter(nil, false)
	b := NewSharedLimiter(nil, false)
	ctx := context.Background()
	const limit = 2

	for _, s := range []*SharedLimiter{a, b} {
		got := 0
		for i := 0; i < 4; i++ {
			d, _ := s.Allow(ctx, "k", limit, time.Minute)
			if d.Allowed {
				got++
			}
		}
		if got != limit {
			t.Fatalf("each disabled limiter should independently allow %d, got %d", limit, got)
		}
	}
}

func TestSharedLimiter_NoLimitAllowsAll(t *testing.T) {
	s := NewSharedLimiter(nil, false)
	d, err := s.Allow(context.Background(), "k", 0, time.Minute)
	if err != nil || !d.Allowed {
		t.Fatalf("limit<=0 should always allow; got allowed=%v err=%v", d.Allowed, err)
	}
}

func TestSharedLimiter_RedisDownDegradesToLocalNotOpen(t *testing.T) {
	_, mr := newTestRedis(t)
	// Fail-fast client so the degradation path doesn't sit through go-redis's
	// default dial backoff once the server is gone.
	rc := redis.NewClient(&redis.Options{
		Addr:        mr.Addr(),
		MaxRetries:  -1,
		DialTimeout: 50 * time.Millisecond,
	})
	t.Cleanup(func() { _ = rc.Close() })
	s := NewSharedLimiter(rc, true)
	mr.Close() // Redis is now unreachable

	ctx := context.Background()
	const limit = 3
	allowed := 0
	for i := 0; i < 6; i++ {
		d, err := s.Allow(ctx, "k", limit, time.Minute)
		if err != nil {
			t.Fatalf("Allow should not surface an error on degradation, got %v", err)
		}
		if d.Allowed {
			allowed++
		}
	}
	// Degraded mode enforces the limit per-instance — NOT fully open.
	if allowed != limit {
		t.Fatalf("with Redis down, expected per-instance limiting to allow %d, got %d", limit, allowed)
	}
}
