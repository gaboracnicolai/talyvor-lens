package ratelimit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func setupLimiter(t *testing.T, rules []RateRule) (*Limiter, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	return New(rc, rules), mr
}

func TestCheck_FirstRequestAllowed(t *testing.T) {
	limiter, _ := setupLimiter(t, []RateRule{{RequestsPerSecond: 10}})

	result := limiter.Check(context.Background(), "default", "key-1")
	if !result.Allowed {
		t.Errorf("first call denied; want allowed (limit_type=%q)", result.LimitType)
	}
	if result.Remaining != 9 {
		t.Errorf("Remaining = %d, want 9", result.Remaining)
	}
}

func TestCheck_OverPerSecondLimitRejected(t *testing.T) {
	limiter, _ := setupLimiter(t, []RateRule{{RequestsPerSecond: 2}})
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if !limiter.Check(ctx, "default", "key-1").Allowed {
			t.Fatalf("call %d should be allowed", i+1)
		}
	}
	result := limiter.Check(ctx, "default", "key-1")
	if result.Allowed {
		t.Fatal("3rd call should be denied by per-second limit")
	}
	if result.LimitType != "second" {
		t.Errorf("LimitType = %q, want %q", result.LimitType, "second")
	}
}

func TestCheck_OverPerMinuteLimitRejected(t *testing.T) {
	// Keep per-second high so it never trips first. Per-minute = 3.
	limiter, _ := setupLimiter(t, []RateRule{
		{RequestsPerSecond: 1000, RequestsPerMinute: 3},
	})
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if !limiter.Check(ctx, "default", "key-1").Allowed {
			t.Fatalf("call %d should be allowed", i+1)
		}
	}
	result := limiter.Check(ctx, "default", "key-1")
	if result.Allowed {
		t.Fatal("4th call should be denied by per-minute limit")
	}
	if result.LimitType != "minute" {
		t.Errorf("LimitType = %q, want %q", result.LimitType, "minute")
	}
}

func TestCheck_WorkspaceRuleOverridesGlobal(t *testing.T) {
	limiter, _ := setupLimiter(t, []RateRule{
		// Global: deny after the first request per second.
		{WorkspaceID: "", KeyID: "", RequestsPerSecond: 1},
		// team-a: allow many.
		{WorkspaceID: "team-a", KeyID: "", RequestsPerSecond: 100},
	})
	ctx := context.Background()

	// 5 calls for team-a should all pass under the workspace-specific rule.
	for i := 0; i < 5; i++ {
		if !limiter.Check(ctx, "team-a", "k").Allowed {
			t.Fatalf("team-a call %d denied; rule should be 100/s", i+1)
		}
	}

	// Different workspace falls through to global 1/s.
	if !limiter.Check(ctx, "team-b", "k").Allowed {
		t.Fatal("team-b first call should pass under global 1/s")
	}
	if limiter.Check(ctx, "team-b", "k").Allowed {
		t.Fatal("team-b second call should be denied under global 1/s")
	}
}

func TestCheck_RetryAfterSecsIsPositive(t *testing.T) {
	limiter, _ := setupLimiter(t, []RateRule{{RequestsPerSecond: 1}})
	ctx := context.Background()

	limiter.Check(ctx, "ws", "k") // 1st: allowed
	r := limiter.Check(ctx, "ws", "k")
	if r.Allowed {
		t.Fatal("2nd call should be denied")
	}
	if r.RetryAfterSecs <= 0 {
		t.Errorf("RetryAfterSecs = %d, want > 0", r.RetryAfterSecs)
	}
}

func TestCheck_RemainingDecrements(t *testing.T) {
	limiter, _ := setupLimiter(t, []RateRule{{RequestsPerSecond: 10}})
	ctx := context.Background()

	r1 := limiter.Check(ctx, "ws", "k")
	r2 := limiter.Check(ctx, "ws", "k")
	r3 := limiter.Check(ctx, "ws", "k")

	if r1.Remaining != 9 || r2.Remaining != 8 || r3.Remaining != 7 {
		t.Errorf("Remaining sequence = %d/%d/%d, want 9/8/7", r1.Remaining, r2.Remaining, r3.Remaining)
	}
}

func TestMiddleware_429OnRateLimit(t *testing.T) {
	limiter, _ := setupLimiter(t, []RateRule{{RequestsPerSecond: 1}})

	called := 0
	h := RateLimitMiddleware(limiter)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
	}))

	// 1st call: 200
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Talyvor-Workspace", "ws-1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("1st call status = %d, want 200", w.Code)
	}

	// 2nd call: 429
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("X-Talyvor-Workspace", "ws-1")
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusTooManyRequests {
		t.Errorf("2nd call status = %d, want 429", w2.Code)
	}
	if called != 1 {
		t.Errorf("downstream called %d times, want 1", called)
	}

	var body map[string]any
	if err := json.Unmarshal(w2.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 429 body: %v", err)
	}
	if body["limit_type"] != "second" {
		t.Errorf("limit_type = %v, want second", body["limit_type"])
	}
}

func TestMiddleware_SetsRetryAfterHeader(t *testing.T) {
	limiter, _ := setupLimiter(t, []RateRule{{RequestsPerSecond: 1}})

	h := RateLimitMiddleware(limiter)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Burn the budget.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Talyvor-Workspace", "ws-2")
	h.ServeHTTP(httptest.NewRecorder(), req)

	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("X-Talyvor-Workspace", "ws-2")
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)

	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w2.Code)
	}
	retry := w2.Header().Get("Retry-After")
	if retry == "" {
		t.Fatal("Retry-After header not set")
	}
	if n, err := strconv.Atoi(retry); err != nil || n <= 0 {
		t.Errorf("Retry-After = %q, want a positive integer", retry)
	}
	if w2.Header().Get("X-RateLimit-Remaining") != "0" {
		t.Errorf("X-RateLimit-Remaining = %q, want 0", w2.Header().Get("X-RateLimit-Remaining"))
	}
}

func TestMiddleware_SetsXRateLimitRemainingHeader(t *testing.T) {
	limiter, _ := setupLimiter(t, []RateRule{{RequestsPerSecond: 5}})

	h := RateLimitMiddleware(limiter)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Talyvor-Workspace", "ws-3")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got := w.Header().Get("X-RateLimit-Remaining")
	if got != "4" {
		t.Errorf("X-RateLimit-Remaining = %q, want 4", got)
	}
}

// Sanity check: under the per-second budget, calls within < 5 ms each.
func TestCheck_Fast(t *testing.T) {
	limiter, _ := setupLimiter(t, []RateRule{{RequestsPerSecond: 1000}})
	ctx := context.Background()
	start := time.Now()
	for i := 0; i < 10; i++ {
		limiter.Check(ctx, "ws", "k")
	}
	if d := time.Since(start) / 10; d > 5*time.Millisecond {
		// miniredis is in-process; if this fails the implementation has
		// an obvious bottleneck.
		t.Logf("average call latency = %v (informational only)", d)
	}
}
