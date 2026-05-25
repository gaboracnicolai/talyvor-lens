package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// setupBucketRedis wires miniredis + a go-redis client for the
// token-bucket tests. Returns (client, close).
func setupBucketRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	return rc
}

// ─── CheckAndConsume (Redis) ─────────────────────

func TestCheckAndConsume_AllowsUnderLimit(t *testing.T) {
	rdb := setupBucketRedis(t)
	ctx := context.Background()
	// 10-token bucket, refilling at 1/sec; first call should pass.
	res, err := CheckAndConsume(ctx, rdb, "test:allow", 10, 1, 1)
	if err != nil {
		t.Fatalf("CheckAndConsume: %v", err)
	}
	if !res.Allowed {
		t.Fatal("first call should be allowed")
	}
	if res.Remaining < 8.9 {
		t.Fatalf("expected ~9 remaining, got %f", res.Remaining)
	}
}

func TestCheckAndConsume_DeniesOverLimit(t *testing.T) {
	rdb := setupBucketRedis(t)
	ctx := context.Background()
	// 3-token bucket, slow refill. Burn through it and then expect denial.
	for i := 0; i < 3; i++ {
		res, err := CheckAndConsume(ctx, rdb, "test:over", 3, 0.1, 1)
		if err != nil {
			t.Fatalf("CheckAndConsume[%d]: %v", i, err)
		}
		if !res.Allowed {
			t.Fatalf("call %d should have been allowed", i)
		}
	}
	res, err := CheckAndConsume(ctx, rdb, "test:over", 3, 0.1, 1)
	if err != nil {
		t.Fatalf("CheckAndConsume: %v", err)
	}
	if res.Allowed {
		t.Fatal("4th call should have been denied")
	}
}

func TestCheckAndConsume_ComputesRetryAfter(t *testing.T) {
	rdb := setupBucketRedis(t)
	ctx := context.Background()
	// 1-token bucket, 1/sec refill. Drain, then check RetryAfter.
	_, _ = CheckAndConsume(ctx, rdb, "test:retry", 1, 1, 1)
	res, _ := CheckAndConsume(ctx, rdb, "test:retry", 1, 1, 1)
	if res.Allowed {
		t.Fatal("expected denial")
	}
	if res.RetryAfter <= 0 {
		t.Fatalf("RetryAfter should be positive, got %v", res.RetryAfter)
	}
	if res.RetryAfter > 5*time.Second {
		t.Fatalf("RetryAfter unexpectedly large: %v", res.RetryAfter)
	}
}

// ─── InMemoryFallback ────────────────────────────

func TestInMemoryFallback_Allows(t *testing.T) {
	f := NewInMemoryFallback()
	res := f.CheckAndConsume("ws_a", 10, 1, 1)
	if !res.Allowed {
		t.Fatal("expected allow on first call")
	}
}

func TestInMemoryFallback_DeniesAfterCapacity(t *testing.T) {
	f := NewInMemoryFallback()
	for i := 0; i < 5; i++ {
		if r := f.CheckAndConsume("ws_b", 5, 0.01, 1); !r.Allowed {
			t.Fatalf("call %d denied unexpectedly", i)
		}
	}
	r := f.CheckAndConsume("ws_b", 5, 0.01, 1)
	if r.Allowed {
		t.Fatal("expected denial after capacity exhausted")
	}
	if r.RetryAfter <= 0 {
		t.Fatal("expected positive RetryAfter on denial")
	}
}

// ─── TPM helpers ─────────────────────────────────

func TestRecordTokens_UpdatesCount(t *testing.T) {
	rdb := setupBucketRedis(t)
	ctx := context.Background()
	if err := RecordTokens(ctx, rdb, "ws_tpm", 500); err != nil {
		t.Fatalf("RecordTokens: %v", err)
	}
	if err := RecordTokens(ctx, rdb, "ws_tpm", 1500); err != nil {
		t.Fatalf("RecordTokens: %v", err)
	}
	ok, remaining, err := CheckTokenBudget(ctx, rdb, "ws_tpm", 10000)
	if err != nil {
		t.Fatalf("CheckTokenBudget: %v", err)
	}
	if !ok {
		t.Fatal("expected budget headroom for 2000 used / 10000 cap")
	}
	if remaining != 8000 {
		t.Fatalf("expected 8000 remaining, got %d", remaining)
	}
}

func TestCheckTokenBudget_DeniesWhenOverTPM(t *testing.T) {
	rdb := setupBucketRedis(t)
	ctx := context.Background()
	_ = RecordTokens(ctx, rdb, "ws_over", 1_000_000)
	ok, remaining, err := CheckTokenBudget(ctx, rdb, "ws_over", 100_000)
	if err != nil {
		t.Fatalf("CheckTokenBudget: %v", err)
	}
	if ok {
		t.Fatal("expected denial when used > cap")
	}
	if remaining != 0 {
		t.Fatalf("expected 0 remaining, got %d", remaining)
	}
}

// ─── multi-tier wiring ───────────────────────────

func TestMultiTier_AppliesMostRestrictiveLimit(t *testing.T) {
	rdb := setupBucketRedis(t)
	ctx := context.Background()
	// Global: 1000 RPM, but workspace caps at 5 RPM.
	limiter := NewMultiTierLimiter(rdb,
		LimitTier{Name: "global", Key: "global", RPM: 1000},
		LimitTier{Name: "workspace", Key: "ws:tight", RPM: 5},
	)
	// First 5 calls succeed (workspace burst = 5 * 1.5 = 7);
	// after 7 calls, the workspace tier should deny.
	allowed := 0
	for i := 0; i < 12; i++ {
		res, _ := limiter.CheckRPM(ctx)
		if res.Allowed {
			allowed++
		}
	}
	if allowed > 8 {
		t.Fatalf("workspace tier should have capped earlier; got %d allowed", allowed)
	}
}

func TestMultiTier_FallbackOnRedisError(t *testing.T) {
	// nil client triggers the in-memory fallback path inside CheckRPM.
	limiter := NewMultiTierLimiter(nil,
		LimitTier{Name: "workspace", Key: "ws:fb", RPM: 10},
	)
	res, _ := limiter.CheckRPM(context.Background())
	if !res.Allowed {
		t.Fatal("fallback should allow first call")
	}
}

// ─── ResetUnix / RetryAfterSeconds helpers ──────

func TestLimitResult_RetryAfterSeconds_Rounding(t *testing.T) {
	if got := (LimitResult{RetryAfter: 0}).RetryAfterSeconds(); got != 0 {
		t.Fatalf("zero retry → 0, got %d", got)
	}
	if got := (LimitResult{RetryAfter: 500 * time.Millisecond}).RetryAfterSeconds(); got != 1 {
		t.Fatalf("rounds up to 1 second, got %d", got)
	}
	if got := (LimitResult{RetryAfter: 12 * time.Second}).RetryAfterSeconds(); got != 12 {
		t.Fatalf("preserves whole seconds, got %d", got)
	}
}
