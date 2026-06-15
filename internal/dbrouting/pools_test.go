package dbrouting

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// These are pure pointer-identity tests — ReadPool never dials, so zero-value
// pool pointers are a safe, DB-free stand-in for "two distinct pools".

// TestReadPool_OffByDefault_ReturnsPrimary — the flip-OFF path (the default):
// LENS_DB_REPLICA_URL unset → main.go leaves replica nil → every analytics read
// transparently uses the primary, identical to single-pool behavior today.
func TestReadPool_OffByDefault_ReturnsPrimary(t *testing.T) {
	primary := &pgxpool.Pool{}
	if got := ReadPool(primary, nil); got != primary {
		t.Fatal("nil replica must resolve to the primary pool (off-by-default)")
	}
}

// TestReadPool_Configured_ReturnsReplica — the flip-ON path: a configured
// replica is used for analytics reads, and never silently falls back to primary.
func TestReadPool_Configured_ReturnsReplica(t *testing.T) {
	primary, replica := &pgxpool.Pool{}, &pgxpool.Pool{}
	if got := ReadPool(primary, replica); got != replica {
		t.Fatal("a configured replica must be returned for analytics reads")
	}
	if ReadPool(primary, replica) == primary {
		t.Fatal("must not return primary when a distinct replica is configured")
	}
}
