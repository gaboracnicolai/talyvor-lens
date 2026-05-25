package retry

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ─── Policy.NextDelay ────────────────────────────

func TestNextDelay_IncreasesExponentially(t *testing.T) {
	p := Policy{InitialDelay: 100 * time.Millisecond, MaxDelay: time.Minute, Multiplier: 2, Jitter: 0}
	a, b, c := p.NextDelay(0), p.NextDelay(1), p.NextDelay(2)
	if a >= b || b >= c {
		t.Fatalf("expected monotonically increasing delays, got %v %v %v", a, b, c)
	}
}

func TestNextDelay_RespectsMaxDelay(t *testing.T) {
	p := Policy{InitialDelay: time.Second, MaxDelay: 5 * time.Second, Multiplier: 10, Jitter: 0}
	d := p.NextDelay(5) // would otherwise be 10^5 seconds
	if d > p.MaxDelay {
		t.Fatalf("expected delay capped at MaxDelay, got %v", d)
	}
}

func TestNextDelay_AppliesJitterInRange(t *testing.T) {
	p := Policy{InitialDelay: 100 * time.Millisecond, MaxDelay: time.Second, Multiplier: 2, Jitter: 0.5}
	// With jitter=0.5 around base 100ms, valid range is [50ms, 150ms].
	for i := 0; i < 30; i++ {
		d := p.NextDelay(0)
		if d < 50*time.Millisecond || d > 150*time.Millisecond {
			t.Fatalf("delay %v outside jitter range", d)
		}
	}
}

func TestNextDelay_NeverZero(t *testing.T) {
	p := Policy{InitialDelay: 0, MaxDelay: time.Minute, Multiplier: 1, Jitter: 0}
	if d := p.NextDelay(0); d < minDelay {
		t.Fatalf("expected minimum delay floor, got %v", d)
	}
}

// ─── IsRetryable ─────────────────────────────────

func TestIsRetryable_RetryableStatuses(t *testing.T) {
	for _, code := range []int{429, 500, 502, 503, 504} {
		if !IsRetryable(nil, code) {
			t.Fatalf("expected %d to be retryable", code)
		}
	}
}

func TestIsRetryable_NonRetryableStatuses(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404, 422} {
		if IsRetryable(nil, code) {
			t.Fatalf("expected %d to be non-retryable", code)
		}
	}
}

func TestIsRetryable_ContextCancellation(t *testing.T) {
	if IsRetryable(context.Canceled, 500) {
		t.Fatal("ctx.Canceled must never be retryable")
	}
	if IsRetryable(context.DeadlineExceeded, 500) {
		t.Fatal("ctx.DeadlineExceeded must never be retryable")
	}
}

func TestIsRetryable_NetworkPatterns(t *testing.T) {
	if !IsRetryable(errors.New("dial tcp: connection refused"), 0) {
		t.Fatal("connection refused should be retryable")
	}
	if !IsRetryable(errors.New("unexpected EOF"), 0) {
		t.Fatal("EOF should be retryable")
	}
}

// ─── ParseRetryAfter ─────────────────────────────

func TestParseRetryAfter_Seconds(t *testing.T) {
	if got := ParseRetryAfter("30"); got != 30*time.Second {
		t.Fatalf("expected 30s, got %v", got)
	}
	if got := ParseRetryAfter("0"); got != 0 {
		t.Fatalf("expected 0, got %v", got)
	}
}

func TestParseRetryAfter_HTTPDate(t *testing.T) {
	future := time.Now().Add(45 * time.Second).UTC().Format(time.RFC1123)
	d := ParseRetryAfter(future)
	if d < 30*time.Second || d > 60*time.Second {
		t.Fatalf("expected ~45s from HTTP-date, got %v", d)
	}
}

func TestParseRetryAfter_Garbage(t *testing.T) {
	if d := ParseRetryAfter("never"); d != 0 {
		t.Fatalf("garbage should return 0, got %v", d)
	}
}

// ─── CircuitBreaker ──────────────────────────────

func TestCircuitBreaker_StartsClosed(t *testing.T) {
	cb := NewCircuitBreaker("test", 3, time.Second, 1)
	if cb.State() != CBClosed {
		t.Fatalf("expected closed, got %s", cb.State())
	}
	if !cb.Allow() {
		t.Fatal("closed breaker should allow")
	}
}

func TestCircuitBreaker_OpensAfterThreshold(t *testing.T) {
	cb := NewCircuitBreaker("test", 3, time.Second, 1)
	for i := 0; i < 3; i++ {
		cb.RecordFailure()
	}
	if cb.State() != CBOpen {
		t.Fatalf("expected open after 3 failures, got %s", cb.State())
	}
	if cb.Allow() {
		t.Fatal("open breaker should deny")
	}
}

func TestCircuitBreaker_HalfOpenAfterTimeout(t *testing.T) {
	cb := NewCircuitBreaker("test", 1, 10*time.Millisecond, 1)
	cb.RecordFailure()
	if cb.State() != CBOpen {
		t.Fatalf("expected open, got %s", cb.State())
	}
	time.Sleep(20 * time.Millisecond)
	if !cb.Allow() {
		t.Fatal("after timeout, breaker should allow a probe")
	}
	if cb.State() != CBHalfOpen {
		t.Fatalf("expected half_open after probe, got %s", cb.State())
	}
}

func TestCircuitBreaker_ClosesAfterSuccessThreshold(t *testing.T) {
	cb := NewCircuitBreaker("test", 1, time.Millisecond, 2)
	cb.RecordFailure()
	time.Sleep(5 * time.Millisecond)
	cb.Allow() // → half_open
	cb.RecordSuccess()
	if cb.State() != CBHalfOpen {
		t.Fatalf("expected still half_open after 1 success, got %s", cb.State())
	}
	cb.RecordSuccess()
	if cb.State() != CBClosed {
		t.Fatalf("expected closed after 2 successes, got %s", cb.State())
	}
}

func TestCircuitBreaker_HalfOpenFailureReopens(t *testing.T) {
	cb := NewCircuitBreaker("test", 1, time.Millisecond, 2)
	cb.RecordFailure()
	time.Sleep(5 * time.Millisecond)
	cb.Allow() // → half_open
	cb.RecordFailure()
	if cb.State() != CBOpen {
		t.Fatalf("expected re-open on half-open failure, got %s", cb.State())
	}
}

// ─── Executor ────────────────────────────────────

func TestExecute_RetriesOnRetryableError(t *testing.T) {
	policy := &Policy{
		MaxAttempts:  3,
		InitialDelay: time.Millisecond,
		MaxDelay:     10 * time.Millisecond,
		Multiplier:   2,
		Jitter:       0,
	}
	reg := NewBreakerRegistry(10, time.Minute, 1)
	exec := NewExecutor(policy, reg)
	var calls int32
	res, err := exec.Execute(context.Background(), "openai", func() (interface{}, int, error) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			return nil, 503, errors.New("service unavailable")
		}
		return "ok", 200, nil
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", res.Attempts)
	}
	if res.Value != "ok" {
		t.Fatalf("expected 'ok', got %v", res.Value)
	}
}

func TestExecute_StopsOnNonRetryable(t *testing.T) {
	exec := NewExecutor(&Policy{MaxAttempts: 5, InitialDelay: time.Millisecond}, NewBreakerRegistry(10, time.Minute, 1))
	var calls int32
	_, err := exec.Execute(context.Background(), "openai", func() (interface{}, int, error) {
		atomic.AddInt32(&calls, 1)
		return nil, 400, errors.New("bad request")
	})
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 call on 400, got %d", calls)
	}
}

func TestExecute_StopsOnCircuitOpen(t *testing.T) {
	reg := NewBreakerRegistry(1, time.Minute, 1)
	exec := NewExecutor(&Policy{MaxAttempts: 3, InitialDelay: time.Millisecond}, reg)
	// Prime the breaker: one failure opens it.
	_, _ = exec.Execute(context.Background(), "anthropic", func() (interface{}, int, error) {
		return nil, 500, errors.New("boom")
	})
	if state := reg.GetOrCreate("anthropic").State(); state != CBOpen {
		t.Fatalf("expected open after 1 failure (threshold=1), got %s", state)
	}
	// Subsequent calls should short-circuit.
	res, err := exec.Execute(context.Background(), "anthropic", func() (interface{}, int, error) {
		t.Fatal("fn should not run when breaker is open")
		return nil, 0, nil
	})
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen, got %v", err)
	}
	if !res.CircuitOpen {
		t.Fatal("expected CircuitOpen=true on result")
	}
}

func TestExecute_ContextCancellation(t *testing.T) {
	exec := NewExecutor(&Policy{MaxAttempts: 5, InitialDelay: 50 * time.Millisecond}, NewBreakerRegistry(10, time.Minute, 1))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := exec.Execute(ctx, "openai", func() (interface{}, int, error) {
		return nil, 503, errors.New("transient")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected ctx.Canceled, got %v", err)
	}
}

func TestExecute_RespectsRetryAfter(t *testing.T) {
	exec := NewExecutor(&Policy{
		MaxAttempts:  2,
		InitialDelay: time.Second, // would normally wait 1s
		MaxDelay:     5 * time.Second,
		Multiplier:   2,
		Jitter:       0,
	}, NewBreakerRegistry(10, time.Minute, 1))
	var calls int32
	start := time.Now()
	_, err := exec.Execute(context.Background(), "openai", func() (interface{}, int, error) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			return nil, 429, NewRetryAfterError("0", errors.New("rate limited"))
		}
		return "ok", 200, nil
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Retry-After: "0" should override the 1s policy delay → loop returns fast.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("expected fast retry from Retry-After: 0, got %v", elapsed)
	}
}

// ─── BreakerRegistry ─────────────────────────────

func TestBreakerRegistry_PerProvider(t *testing.T) {
	r := NewBreakerRegistry(2, time.Minute, 1)
	a := r.GetOrCreate("openai")
	b := r.GetOrCreate("anthropic")
	if a == b {
		t.Fatal("expected per-provider breakers, got shared instance")
	}
	a.RecordFailure()
	a.RecordFailure()
	if a.State() != CBOpen {
		t.Fatal("openai should be open")
	}
	if b.State() != CBClosed {
		t.Fatal("anthropic should still be closed")
	}
}

func TestBreakerRegistry_StateChangeHook(t *testing.T) {
	r := NewBreakerRegistry(1, time.Minute, 1)
	var changes []string
	r.SetOnStateChange(func(name string, from, to CBState) {
		changes = append(changes, name+":"+string(from)+"→"+string(to))
	})
	b := r.GetOrCreate("openai")
	b.RecordFailure()
	// hook fires in a goroutine — give it a beat
	time.Sleep(10 * time.Millisecond)
	if len(changes) == 0 || !strings.Contains(changes[0], "closed→open") {
		t.Fatalf("expected closed→open hook, got %v", changes)
	}
}

// ─── HTTP-shape error wrapping ───────────────────

func TestRetryAfterError_UnwrapsAndFormats(t *testing.T) {
	inner := errors.New("rate limited")
	w := NewRetryAfterError("30", inner)
	if w.After != 30*time.Second {
		t.Fatalf("expected 30s, got %v", w.After)
	}
	if !errors.Is(w, inner) {
		t.Fatal("errors.Is should unwrap")
	}
	if w.Error() == "" {
		t.Fatal("non-empty error message expected")
	}
	// Ensure we still satisfy net/http test conventions in unsealing.
	_ = http.StatusTooManyRequests
}
