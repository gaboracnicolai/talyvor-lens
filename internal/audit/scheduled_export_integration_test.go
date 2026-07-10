package audit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func setWatermark(t *testing.T, pool *pgxpool.Pool, at string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE audit_export_state SET last_exported_at = $1 WHERE id = true`, at); err != nil {
		t.Fatalf("set watermark: %v", err)
	}
}

func readWatermark(t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var tm time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT last_exported_at FROM audit_export_state WHERE id = true`).Scan(&tm); err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	return tm
}

// TestScheduledExport_AdvancesWatermarkOnSuccess — a successful sink POST advances
// the watermark to the window's upper bound.
func TestScheduledExport_AdvancesWatermarkOnSuccess(t *testing.T) {
	pool := auditTestPool(t)
	setWatermark(t, pool, "1970-01-01T00:00:00Z")
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	before := readWatermark(t, pool)
	// Inject the test server's client: the production webhook client is SSRF-guarded and blocks the
	// loopback httptest sink by design — this exercises the dispatch without weakening the guard.
	if err := NewScheduledExport(pool, srv.URL).WithHTTPClient(srv.Client()).ExportOnce(context.Background()); err != nil {
		t.Fatalf("ExportOnce: %v", err)
	}
	if posts != 1 {
		t.Errorf("sink should receive exactly 1 POST, got %d", posts)
	}
	if after := readWatermark(t, pool); !after.After(before) {
		t.Errorf("watermark must advance on success: before=%v after=%v", before, after)
	}
}

// TestScheduledExport_NoAdvanceOnSinkFailure — a 5xx sink leaves the watermark
// untouched, so the next run re-covers the gap (at-least-once).
func TestScheduledExport_NoAdvanceOnSinkFailure(t *testing.T) {
	pool := auditTestPool(t)
	setWatermark(t, pool, "2000-01-01T00:00:00Z")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	before := readWatermark(t, pool)
	// Seam in the test client so this exercises the real 5xx-sink failure path, not the SSRF guard
	// (which would also block the loopback sink — but that's not what this test is asserting).
	if err := NewScheduledExport(pool, srv.URL).WithHTTPClient(srv.Client()).ExportOnce(context.Background()); err == nil {
		t.Error("ExportOnce must error on a 5xx sink")
	}
	if after := readWatermark(t, pool); !after.Equal(before) {
		t.Errorf("watermark must NOT advance on sink failure: before=%v after=%v", before, after)
	}
}

// TestScheduledExport_DefaultOffNoLoop — an empty URL never starts the loop
// (default-off). No DB needed: StartLoop returns before touching the pool.
func TestScheduledExport_DefaultOffNoLoop(t *testing.T) {
	se := NewScheduledExport(nil, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { se.StartLoop(ctx, time.Hour); close(done) }()
	select {
	case <-done: // returned immediately — correct
	case <-time.After(2 * time.Second):
		t.Error("StartLoop with an empty URL must return immediately (default off)")
	}
}
