package backpressure

import (
	"sync"
	"sync/atomic"
	"testing"
)

// The cap must hold exactly under concurrent TryAcquire/Release pressure:
// at no instant may more than n holders exist. Run with -race.
func TestLimiter_CapHoldsUnderConcurrency(t *testing.T) {
	const cap = 8
	l := New(cap)

	var inFlight, peak atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if !l.TryAcquire() {
					continue
				}
				cur := inFlight.Add(1)
				// track the high-water mark
				for {
					p := peak.Load()
					if cur <= p || peak.CompareAndSwap(p, cur) {
						break
					}
				}
				inFlight.Add(-1)
				l.Release()
			}
		}()
	}
	wg.Wait()

	if p := peak.Load(); p > cap {
		t.Fatalf("concurrency bound violated: peak %d > cap %d", p, cap)
	}
	// NOTE: deliberately no assertion on Dropped() here — whether collisions
	// occur depends on scheduling (observed flaky). Drop behavior is covered
	// deterministically by TestLimiter_DropAndRecover.
}

// A full limiter sheds; releasing restores admission.
func TestLimiter_DropAndRecover(t *testing.T) {
	l := New(1)
	if !l.TryAcquire() {
		t.Fatal("first acquire must succeed")
	}
	if l.TryAcquire() {
		t.Fatal("second acquire must be shed while the slot is held")
	}
	if l.Dropped() != 1 {
		t.Fatalf("dropped = %d, want 1", l.Dropped())
	}
	l.Release()
	if !l.TryAcquire() {
		t.Fatal("acquire must succeed again after Release")
	}
}

// nil Limiter (bound disabled) admits everything and never logs.
func TestLimiter_NilAdmitsAll(t *testing.T) {
	var l *Limiter
	for i := 0; i < 10; i++ {
		if !l.TryAcquire() {
			t.Fatal("nil limiter must always admit")
		}
		l.Release()
	}
	if l.Dropped() != 0 || l.LogDrop() {
		t.Fatal("nil limiter must report zero drops and never log")
	}
	if New(0) != nil || New(-3) != nil {
		t.Fatal("New(n<=0) must return nil (bound off)")
	}
}

// Unmatched Release must not block or panic.
func TestLimiter_UnmatchedReleaseHarmless(t *testing.T) {
	l := New(2)
	l.Release() // nothing held — must be a no-op
	a1, a2 := l.TryAcquire(), l.TryAcquire()
	if !a1 || !a2 {
		t.Fatal("cap must be intact after an unmatched Release")
	}
	if l.TryAcquire() {
		t.Fatal("cap must still be enforced")
	}
}

// LogDrop samples: first drop logs, then every 256th.
func TestLimiter_LogDropSampling(t *testing.T) {
	l := New(1)
	l.TryAcquire() // occupy the slot

	l.TryAcquire() // drop #1
	if !l.LogDrop() {
		t.Fatal("first drop must log")
	}
	for i := 2; i <= 255; i++ {
		l.TryAcquire()
		if l.LogDrop() {
			t.Fatalf("drop #%d must not log", i)
		}
	}
	l.TryAcquire() // drop #256
	if !l.LogDrop() {
		t.Fatal("drop #256 must log")
	}
}
