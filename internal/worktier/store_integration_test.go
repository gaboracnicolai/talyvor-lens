package worktier

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func wtTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG worktier test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS work_tier_observations`,
		`CREATE TABLE work_tier_observations (
			id BIGSERIAL PRIMARY KEY, workspace_id TEXT NOT NULL,
			feature TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT '',
			size_bucket TEXT NOT NULL, cost_bucket TEXT NOT NULL, complexity TEXT NOT NULL, sensitivity TEXT NOT NULL,
			input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
			cost_usd DOUBLE PRECISION NOT NULL DEFAULT 0, complexity_score INTEGER NOT NULL DEFAULT 0,
			pii_detected BOOLEAN NOT NULL DEFAULT FALSE, guardrail_fired BOOLEAN NOT NULL DEFAULT FALSE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool
}

// TestStore_RoundTrip_RawColumns — Record persists the derived buckets AND the
// raw signal behind each axis (re-bucketable); a direct read confirms every column.
func TestStore_RoundTrip_RawColumns(t *testing.T) {
	pool := wtTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)
	wt := Classify(50000, 200, 0.05, 4, true, false, "metadata") // large, moderate cost, moderate cx, elevated(pii)
	if err := store.Record(ctx, "ws1", "search", "gpt-4o", "openai", wt, 50000, 200, 0.05, 4, true, false); err != nil {
		t.Fatalf("Record: %v", err)
	}
	var sz, cost, cx, sens, model string
	var in, out, cscore int
	var usd float64
	var pii, guard bool
	if err := pool.QueryRow(ctx, `SELECT size_bucket, cost_bucket, complexity, sensitivity, model,
		input_tokens, output_tokens, cost_usd, complexity_score, pii_detected, guardrail_fired
		FROM work_tier_observations WHERE workspace_id='ws1'`).Scan(
		&sz, &cost, &cx, &sens, &model, &in, &out, &usd, &cscore, &pii, &guard); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if sz != "large" || cost != "moderate" || cx != "moderate" || sens != "elevated" {
		t.Errorf("buckets wrong: %s/%s/%s/%s", sz, cost, cx, sens)
	}
	if in != 50000 || out != 200 || cscore != 4 || !pii || guard || usd != 0.05 || model != "gpt-4o" {
		t.Errorf("raw signal not persisted: in=%d out=%d score=%d pii=%v guard=%v usd=%v model=%s", in, out, cscore, pii, guard, usd, model)
	}
}

// TestStore_Aggregate_PerWorkspace_SliceableByModel — the read-side aggregate
// returns the per-workspace tier distribution, grouped by model, and NEVER leaks
// another workspace's rows.
func TestStore_Aggregate_PerWorkspace_SliceableByModel(t *testing.T) {
	pool := wtTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)
	small := Classify(500, 100, 0.0005, 0, false, false, "full") // small/trivial/trivial/normal
	rec := func(ws, model string, n int) {
		for i := 0; i < n; i++ {
			if err := store.Record(ctx, ws, "f", model, "openai", small, 500, 100, 0.0005, 0, false, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	rec("wsA", "gpt-4o", 3)
	rec("wsA", "gpt-4o-mini", 2)
	rec("wsB", "gpt-4o", 5) // a DIFFERENT workspace — must not leak into wsA's aggregate

	got, err := store.Aggregate(ctx, "wsA", 24*time.Hour)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	byModel := map[string]int{}
	for _, tc := range got {
		if tc.Size != SizeSmall { // all are the same small tier here
			t.Errorf("unexpected tier in aggregate: %+v", tc)
		}
		byModel[tc.Model] += tc.Count
	}
	if byModel["gpt-4o"] != 3 || byModel["gpt-4o-mini"] != 2 {
		t.Errorf("per-model counts wrong: %v (want gpt-4o:3, gpt-4o-mini:2)", byModel)
	}
	if _, leaked := byModel["gpt-4o"]; leaked && byModel["gpt-4o"] != 3 {
		t.Error("wsB rows leaked into wsA aggregate")
	}
	// total for wsA must be exactly 5 (3+2), proving wsB's 5 didn't leak.
	total := 0
	for _, n := range byModel {
		total += n
	}
	if total != 5 {
		t.Errorf("wsA aggregate total = %d, want 5 (no cross-tenant leak)", total)
	}
}

// TestStore_NilPool_NoOp — a nil-pool store is inert (Record/Aggregate no-op).
func TestStore_NilPool_NoOp(t *testing.T) {
	s := NewStore(nil)
	if err := s.Record(context.Background(), "ws", "f", "m", "p", WorkTier{}, 1, 1, 0, 0, false, false); err != nil {
		t.Errorf("nil-pool Record must be a no-op, got %v", err)
	}
	if got, err := s.Aggregate(context.Background(), "ws", time.Hour); err != nil || got != nil {
		t.Errorf("nil-pool Aggregate must be empty no-op, got %v err=%v", got, err)
	}
}
