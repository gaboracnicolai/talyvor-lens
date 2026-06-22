package audit

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func seedTokenEvent(t *testing.T, pool *pgxpool.Pool, ws string, age time.Duration) {
	t.Helper()
	// Mirror a real production row: alerts.go binds team/feature/session_id/request_id
	// as Go strings (never NULL), so set them here too. Omitting them leaves NULLs that
	// the off-box exporter (scans into non-nullable string fields) cannot read — which
	// would make a seeded row poison a sibling export test, not reflect production.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO token_events
		   (provider, model, input_tokens, output_tokens, workspace_id, created_at,
		    team, feature, cost_usd, pii_detected, session_id, request_id)
		 VALUES ('p','m',1,1,$1,$2, '', '', 0, false, '', gen_random_uuid()::text)`,
		ws, time.Now().Add(-age)); err != nil {
		t.Fatalf("seed token_events: %v", err)
	}
}

func countWS(t *testing.T, pool *pgxpool.Pool, ws string) int {
	t.Helper()
	var c int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM token_events WHERE workspace_id=$1`, ws).Scan(&c); err != nil {
		t.Fatalf("count: %v", err)
	}
	return c
}

// TestRetention_DeletesOldRetainsNew — rows older than the window are swept (via
// the scoped retention flag the trigger accepts); newer rows are retained.
func TestRetention_DeletesOldRetainsNew(t *testing.T) {
	pool := auditTestPool(t)
	const ws = "ws_ret_age"
	seedTokenEvent(t, pool, ws, 60*24*time.Hour) // old
	seedTokenEvent(t, pool, ws, 1*time.Hour)     // new

	if _, err := NewRetention(pool, 30*24*time.Hour, false, false).SweepOnce(context.Background()); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if c := countWS(t, pool, ws); c != 1 {
		t.Errorf("ws %s: %d rows remain, want 1 (old swept, new retained)", ws, c)
	}
}

// TestRetention_DisabledByDefault — window <= 0 deletes nothing (the default).
func TestRetention_DisabledByDefault(t *testing.T) {
	pool := auditTestPool(t)
	const ws = "ws_ret_disabled"
	seedTokenEvent(t, pool, ws, 60*24*time.Hour)

	n, err := NewRetention(pool, 0, false, false).SweepOnce(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("disabled sweeper must delete nothing: n=%d err=%v", n, err)
	}
	if c := countWS(t, pool, ws); c != 1 {
		t.Errorf("disabled: old row must be retained; count=%d", c)
	}
}

// TestRetention_Batching — 5 old rows with batch size 2 ⇒ the delete loop iterates
// (>=3 batches) and drains them all. Drain-first makes the count deterministic.
func TestRetention_Batching(t *testing.T) {
	pool := auditTestPool(t)
	ctx := context.Background()
	const ws = "ws_ret_batch"
	r := &Retention{pool: pool, window: 30 * 24 * time.Hour, batch: 2, log: slog.Default()}

	if _, err := r.SweepOnce(ctx); err != nil { // drain any pre-existing old rows
		t.Fatalf("pre-drain: %v", err)
	}
	for i := 0; i < 5; i++ {
		seedTokenEvent(t, pool, ws, 60*24*time.Hour)
	}
	n, err := r.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	// >=5 deleted with batch size 2 ⇒ the delete loop iterated >=3 times (a single
	// batch of 2 cannot drain 5). The shared test DB may carry old rows from sibling
	// tests, so n is a floor, not an equality; the per-workspace count is the strict
	// check that this test's rows were fully swept.
	if n < 5 {
		t.Errorf("expected >=5 old rows deleted across batches of 2 (batching), got %d", n)
	}
	if c := countWS(t, pool, ws); c != 0 {
		t.Errorf("all of this test's old rows must be swept; %d remain", c)
	}
}

// wsCreatedAt returns the created_at of the single surviving row for ws (callers
// assert the row count is 1 first).
func wsCreatedAt(t *testing.T, pool *pgxpool.Pool, ws string) time.Time {
	t.Helper()
	var at time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT created_at FROM token_events WHERE workspace_id=$1`, ws).Scan(&at); err != nil {
		t.Fatalf("created_at query: %v", err)
	}
	return at
}

// TestRetention_RequireExport_KeepsUnexported — U14 #187 core property: with
// require-export ON, a row past the retention window is pruned ONLY if it is at/below
// the export watermark. An aged-but-un-exported row (created AFTER the watermark) is
// KEPT — proving prune cannot remove an un-exported row.
func TestRetention_RequireExport_KeepsUnexported(t *testing.T) {
	pool := auditTestPool(t)
	ctx := context.Background()
	const ws = "ws_ret_reqexp_keep"
	seedTokenEvent(t, pool, ws, 90*24*time.Hour) // aged AND exported (older than watermark)
	seedTokenEvent(t, pool, ws, 60*24*time.Hour) // aged but UN-exported (newer than watermark)
	wm := time.Now().Add(-75 * 24 * time.Hour)   // between the two rows
	setWatermark(t, pool, wm.Format(time.RFC3339Nano))

	r := NewRetention(pool, 30*24*time.Hour, true, true) // requireExport + export enabled
	if _, err := r.SweepOnce(ctx); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if c := countWS(t, pool, ws); c != 1 {
		t.Fatalf("want 1 row kept (aged-but-un-exported), got %d", c)
	}
	if got := wsCreatedAt(t, pool, ws); !got.After(wm) {
		t.Errorf("survivor must be the un-exported row (created_at > watermark); created_at=%s watermark=%s", got, wm)
	}
}

// TestRetention_RequireExport_ExportDisabled_Skips — U14 #187: with require-export ON
// but off-box export DISABLED, the sweep prunes NOTHING and warns — it never destroys
// history that cannot be proven exported.
func TestRetention_RequireExport_ExportDisabled_Skips(t *testing.T) {
	pool := auditTestPool(t)
	ctx := context.Background()
	const ws = "ws_ret_reqexp_noexport"
	seedTokenEvent(t, pool, ws, 90*24*time.Hour) // aged: age-only would sweep it

	var buf bytes.Buffer
	r := NewRetention(pool, 30*24*time.Hour, true, false) // requireExport ON, export DISABLED
	r.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	n, err := r.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if n != 0 {
		t.Errorf("require-export + export disabled must delete nothing, deleted %d", n)
	}
	if c := countWS(t, pool, ws); c != 1 {
		t.Errorf("aged row must be KEPT when export is disabled, %d remain", c)
	}
	if !strings.Contains(buf.String(), "SKIPPED") {
		t.Errorf("expected a SKIPPED warning when export is disabled; logs: %q", buf.String())
	}
}

// TestRetention_RequireExportOff_AgeOnly — default-off is today's behaviour: age-only
// pruning that does NOT consult the watermark. The watermark is pinned at epoch (which,
// if wrongly consulted, would prune NOTHING) yet the aged row is still swept — proving
// the export gate is fully bypassed when off, with zero behaviour change.
func TestRetention_RequireExportOff_AgeOnly(t *testing.T) {
	pool := auditTestPool(t)
	ctx := context.Background()
	const ws = "ws_ret_default_ageonly"
	seedTokenEvent(t, pool, ws, 60*24*time.Hour)  // old (past the 30d window)
	seedTokenEvent(t, pool, ws, 1*time.Hour)      // new
	setWatermark(t, pool, "1970-01-01T00:00:00Z") // epoch: if (wrongly) consulted, nothing prunes

	r := NewRetention(pool, 30*24*time.Hour, false, true) // requireExport OFF
	if _, err := r.SweepOnce(ctx); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if c := countWS(t, pool, ws); c != 1 {
		t.Fatalf("default-off age-only: want 1 (old swept, new kept), got %d", c)
	}
	if got := wsCreatedAt(t, pool, ws); time.Since(got) > 30*24*time.Hour {
		t.Errorf("survivor should be the new (<30d) row — old must be swept despite epoch watermark; age=%v", time.Since(got))
	}
}
