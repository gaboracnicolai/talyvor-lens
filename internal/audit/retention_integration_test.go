package audit

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func seedTokenEvent(t *testing.T, pool *pgxpool.Pool, ws string, age time.Duration) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO token_events (provider,model,input_tokens,output_tokens,workspace_id,created_at)
		 VALUES ('p','m',1,1,$1,$2)`, ws, time.Now().Add(-age)); err != nil {
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

	if _, err := NewRetention(pool, 30*24*time.Hour).SweepOnce(context.Background()); err != nil {
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

	n, err := NewRetention(pool, 0).SweepOnce(context.Background())
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
