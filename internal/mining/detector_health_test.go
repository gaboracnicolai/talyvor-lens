package mining

import (
	"testing"
	"time"
)

// TestDetectorHealth_StallIsVisible is the Phase-4a Item 4 RED-first proof: a
// healthy detector (ran within its window) reports healthy; a stalled one (last
// run older than maxStale) reports UNHEALTHY — so a prolonged detector outage is
// never SILENT (an operator can alert on it). The accepted trade is that the
// fail-closed layer strands mints during the stall (recoverable) rather than
// settling blind — but the stall must be observable.
func TestDetectorHealth_StallIsVisible(t *testing.T) {
	h := NewDetectorHealth("traffic-pattern", 5*time.Minute)

	// Never run ⇒ unhealthy (no heartbeat yet).
	if h.Healthy() {
		t.Error("a detector that never ran must be UNHEALTHY (no heartbeat)")
	}

	// A fresh run ⇒ healthy.
	h.MarkRun()
	if !h.Healthy() {
		t.Fatal("a detector that just ran must be healthy")
	}
	if time.Since(h.LastRun()) > time.Second {
		t.Error("LastRun must reflect the recent run")
	}

	// Simulate a stall: last run 10 minutes ago (> 5m maxStale) ⇒ unhealthy + visible age.
	h.markRunAt(time.Now().Add(-10 * time.Minute))
	if h.Healthy() {
		t.Fatal("a stalled detector (last run 10m ago, maxStale 5m) must be UNHEALTHY")
	}
	if age := h.StaleFor(); age < 9*time.Minute {
		t.Errorf("StaleFor=%v, want ≈10m (the stall must be quantified/visible)", age)
	}
}
