package dbrouting

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
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
	// B37.11: no detail, because /healthz reads a healthy check with a detail as degraded.
	ok, _, detail := m.Check(context.Background())
	if !ok || detail != "" {
		t.Errorf("nil-replica Check must be a plain healthy no-op; got ok=%v detail=%q", ok, detail)
	}
}

// TestLagMonitor_UnreachableReplica_Unhealthy — a configured replica nobody can reach still reads
// unhealthy (B37.11 changes only the no-replica case).
func TestLagMonitor_UnreachableReplica_Unhealthy(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://lens@127.0.0.1:1/lens?connect_timeout=1")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()
	ok, _, detail := NewLagMonitor(pool, nil, 0, nil).Check(context.Background())
	if ok || detail == "" {
		t.Errorf("an unreachable replica must read unhealthy with its error; got ok=%v detail=%q", ok, detail)
	}
}
