package dbrouting

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// TestOpenReplica_Empty_ReturnsNil — the default (LENS_DB_REPLICA_URL unset):
// no replica, no WARN, analytics fall back to primary via ReadPool.
func TestOpenReplica_Empty_ReturnsNil(t *testing.T) {
	if got := OpenReplica(context.Background(), "", ReplicaOpts{}); got != nil {
		got.Close()
		t.Fatal("empty DSN must yield a nil replica (off-by-default)")
	}
}

// TestOpenReplica_Malformed_FallsBackToPrimary — a malformed DSN must NOT crash
// boot; it returns nil (→ primary via ReadPool) and logs a WARN.
func TestOpenReplica_Malformed_FallsBackToPrimary(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	got := OpenReplica(context.Background(), "postgres://localhost:not_a_port/db", ReplicaOpts{Log: log})
	if got != nil {
		got.Close()
		t.Fatal("malformed DSN must fall back to nil/primary, not crash")
	}
	if !strings.Contains(buf.String(), "read-replica DSN invalid") {
		t.Errorf("malformed DSN must log a WARN; got %q", buf.String())
	}
}

// TestOpenReplica_Unreachable_FallsBackToPrimary — a syntactically valid DSN
// pointing at a dead endpoint pings-fail → nil (→ primary) + WARN, never a
// crash. 127.0.0.1:1 → connection refused, fast (no network wait).
func TestOpenReplica_Unreachable_FallsBackToPrimary(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	dsn := "postgres://postgres:x@127.0.0.1:1/lens_test?sslmode=disable"
	got := OpenReplica(context.Background(), dsn, ReplicaOpts{Log: log})
	if got != nil {
		got.Close()
		t.Fatal("unreachable replica must fall back to nil/primary, not crash")
	}
	if !strings.Contains(buf.String(), "read-replica ping failed") {
		t.Errorf("unreachable replica must log a ping-failed WARN; got %q", buf.String())
	}
}
