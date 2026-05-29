package retry

import (
	"sync"
	"testing"
	"time"
)

// ApplyRemoteState exists so the HA breaker-gossip layer can mirror a peer's
// transition into this instance's breaker. It must NOT fire onStateChange —
// that hook is the publish path, and re-firing it on a mirrored transition
// would echo the event back onto the bus forever.

func TestApplyRemoteState_SetsStateWithoutFiringCallback(t *testing.T) {
	cb := NewCircuitBreaker("openai", 5, time.Minute, 2)

	var mu sync.Mutex
	fired := 0
	cb.SetOnStateChange(func(_ string, _, _ CBState) {
		mu.Lock()
		fired++
		mu.Unlock()
	})

	cb.ApplyRemoteState(CBOpen)

	if got := cb.State(); got != CBOpen {
		t.Fatalf("state = %q, want open", got)
	}
	// Give any (erroneous) goroutine callback a chance to run.
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if fired != 0 {
		t.Fatalf("onStateChange fired %d times on ApplyRemoteState; want 0 (would echo on the gossip bus)", fired)
	}
}

func TestApplyRemoteState_OpenHonorsResetTimeoutBeforeProbe(t *testing.T) {
	cb := NewCircuitBreaker("openai", 5, 10*time.Millisecond, 2)
	cb.ApplyRemoteState(CBOpen)

	if cb.Allow() {
		t.Fatal("breaker mirrored to open should refuse immediately")
	}
	time.Sleep(20 * time.Millisecond)
	if !cb.Allow() {
		t.Fatal("after reset timeout, mirrored-open breaker should allow a probe")
	}
}

func TestRegistryApplyRemoteState_MirrorsAcrossProviders(t *testing.T) {
	r := NewBreakerRegistry(5, time.Minute, 2)
	r.ApplyRemoteState("anthropic", CBOpen)

	if got := r.GetOrCreate("anthropic").State(); got != CBOpen {
		t.Fatalf("anthropic state = %q, want open", got)
	}
	// A provider we never heard about stays closed.
	if got := r.GetOrCreate("openai").State(); got != CBClosed {
		t.Fatalf("openai state = %q, want closed", got)
	}
}
