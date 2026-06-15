package dbrouting

import (
	"context"
	"testing"
)

// TestLagMonitor_NoReplica_NoOp — with no replica configured: Start spawns no
// goroutine (publish never fires → the gauge stays 0) and Check is a healthy
// no-op (never trips /healthz). The feature is OFF, not broken.
func TestLagMonitor_NoReplica_NoOp(t *testing.T) {
	published := false
	m := NewLagMonitor(nil, func(float64) { published = true }, 0, nil)
	m.Start(context.Background())
	if published {
		t.Error("a nil-replica monitor must never publish a lag value (gauge stays 0)")
	}
	ok, _, detail := m.Check(context.Background())
	if !ok || detail != "no replica configured" {
		t.Errorf("nil-replica Check must be a healthy no-op; got ok=%v detail=%q", ok, detail)
	}
}
