package poolroyalty

import (
	"context"
	"testing"
)

// (proof 1, unit half) INERT BY DEFAULT: rate 0 ⇒ NewHeldBenchmarkAnchor refuses ⇒ anchor nil ⇒ RunOnce
// is a TOTAL no-op even with the enable flag ON and a nil DB (proving it touches no DB). A positive rate
// DOES construct the anchor — so the only thing between inert and live is the rate (+ the flags).
func TestRoutingPredictionMinter_InertNoDBAccess(t *testing.T) {
	m := NewRoutingPredictionMinter(nil, nil, 0, func() bool { return true }) // rate 0, "both flags on"
	if m.anchor != nil {
		t.Fatal("rate 0 must leave the anchor nil (inert) — NewHeldBenchmarkAnchor refuses a non-positive rate")
	}
	// nil db: if RunOnce tried to query, it would panic — so reaching (0,nil) PROVES zero DB access.
	n, err := m.RunOnce(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("rate-0 minter must no-op (no DB access, no mint): n=%d err=%v", n, err)
	}

	// A positive rate constructs the anchor (live path is then one flag + rate away).
	live := NewRoutingPredictionMinter(nil, nil, 10, func() bool { return true })
	if live.anchor == nil {
		t.Fatal("a positive rate must construct the held-benchmark anchor")
	}

	// Flag OFF with a positive rate is still inert (the enabled() gate short-circuits before any DB).
	off := NewRoutingPredictionMinter(nil, nil, 10, func() bool { return false })
	if n, err := off.RunOnce(context.Background()); err != nil || n != 0 {
		t.Fatalf("flag-off minter must no-op even with a positive rate: n=%d err=%v", n, err)
	}
}
