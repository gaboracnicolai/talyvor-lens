package ha

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/talyvor/lens/internal/retry"
)

// twoInstanceBreakers builds two independent breaker registries + gossip
// publishers/subscribers wired to the same miniredis, modelling two pods.
func twoInstanceBreakers(t *testing.T, enabled bool) (a, b *retry.BreakerRegistry, sbA, sbB *SharedBreaker) {
	t.Helper()
	_, mr := newTestRedis(t)

	newClient := func() *redis.Client {
		rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = rc.Close() })
		return rc
	}

	a = retry.NewBreakerRegistry(2, time.Minute, 1)
	b = retry.NewBreakerRegistry(2, time.Minute, 1)
	sbA = NewSharedBreaker(newClient(), a, "instance-A", enabled)
	sbB = NewSharedBreaker(newClient(), b, "instance-B", enabled)

	// A's local transitions get gossiped.
	a.SetOnStateChange(func(name string, _, to retry.CBState) { sbA.Publish(name, to) })
	b.SetOnStateChange(func(name string, _, to retry.CBState) { sbB.Publish(name, to) })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := sbA.Start(ctx); err != nil {
		t.Fatalf("sbA.Start: %v", err)
	}
	if err := sbB.Start(ctx); err != nil {
		t.Fatalf("sbB.Start: %v", err)
	}
	t.Cleanup(func() { _ = sbA.Close(); _ = sbB.Close() })
	return a, b, sbA, sbB
}

func waitForState(t *testing.T, reg *retry.BreakerRegistry, provider string, want retry.CBState) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if reg.GetOrCreate(provider).State() == want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func TestSharedBreaker_TripOnOneInstanceMirroredByPeer(t *testing.T) {
	regA, regB, _, _ := twoInstanceBreakers(t, true)

	// Trip "openai" on instance A (threshold is 2).
	cb := regA.GetOrCreate("openai")
	cb.RecordFailure()
	cb.RecordFailure()
	if cb.State() != retry.CBOpen {
		t.Fatalf("precondition: A's breaker should be open, got %q", cb.State())
	}

	if !waitForState(t, regB, "openai", retry.CBOpen) {
		t.Fatal("peer instance B did not mirror the open transition within the deadline")
	}
}

func TestSharedBreaker_DisabledDoesNotPropagate(t *testing.T) {
	regA, regB, _, _ := twoInstanceBreakers(t, false)

	cb := regA.GetOrCreate("openai")
	cb.RecordFailure()
	cb.RecordFailure()
	if cb.State() != retry.CBOpen {
		t.Fatalf("precondition: A's breaker should be open, got %q", cb.State())
	}

	// Give any (incorrect) propagation a chance to happen, then assert B is
	// still closed — disabled mode is strictly local.
	time.Sleep(200 * time.Millisecond)
	if got := regB.GetOrCreate("openai").State(); got != retry.CBClosed {
		t.Fatalf("disabled mode leaked a transition to peer B: state = %q, want closed", got)
	}
}
