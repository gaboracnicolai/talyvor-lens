package nodelatency

import (
	"context"
	"math"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func newTestStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG node-latency test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), `CREATE TABLE IF NOT EXISTS node_cohort_latency_stats (
		node_id TEXT NOT NULL, feature_category TEXT NOT NULL, input_token_range TEXT NOT NULL,
		complexity_bucket TEXT NOT NULL, model TEXT NOT NULL, latency_ewma DOUBLE PRECISION NOT NULL DEFAULT 0,
		cost_weight_accum DOUBLE PRECISION NOT NULL DEFAULT 0, sample_count BIGINT NOT NULL DEFAULT 0,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (node_id, feature_category, input_token_range, complexity_bucket, model))`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `TRUNCATE node_cohort_latency_stats`); err != nil {
		t.Fatal(err)
	}
	return &Store{db: pool}, pool
}

func rowOf(t *testing.T, pool *pgxpool.Pool, node, feat, itr, cb, model string) (ewma, cost float64, n int64) {
	t.Helper()
	_ = pool.QueryRow(context.Background(),
		`SELECT latency_ewma, cost_weight_accum, sample_count FROM node_cohort_latency_stats
		 WHERE node_id=$1 AND feature_category=$2 AND input_token_range=$3 AND complexity_bucket=$4 AND model=$5`,
		node, feat, itr, cb, model).Scan(&ewma, &cost, &n)
	return
}

// (proof 1) Capture correctness + the α=0.2 EWMA blend at the (node,cohort,MODEL) grain: a second serve on
// the SAME (node,cohort,model) blends latency via 0.2 and accumulates cost + count.
func TestRecordServe_EWMABlendAndAccumulation(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	if err := s.RecordServe(ctx, "nodeA", "chat", "small", "trivial", "gpt-4o", 100, 3); err != nil {
		t.Fatal(err)
	}
	ewma, cost, n := rowOf(t, pool, "nodeA", "chat", "small", "trivial", "gpt-4o")
	if ewma != 100 || cost != 3 || n != 1 {
		t.Fatalf("seed: got ewma=%.3f cost=%.1f n=%d, want 100/3/1", ewma, cost, n)
	}

	// Same (node,cohort,model): latency 200, cost 4 → ewma = 100*0.8 + 200*0.2 = 120; cost 7; n=2.
	if err := s.RecordServe(ctx, "nodeA", "chat", "small", "trivial", "gpt-4o", 200, 4); err != nil {
		t.Fatal(err)
	}
	ewma, cost, n = rowOf(t, pool, "nodeA", "chat", "small", "trivial", "gpt-4o")
	if math.Abs(ewma-120) > 1e-9 || cost != 7 || n != 2 {
		t.Fatalf("blend: got ewma=%.6f cost=%.1f n=%d, want 120/7/2 (100*0.8+200*0.2)", ewma, cost, n)
	}
}

// (proof — the GRAIN) two serves of the SAME (node,cohort) but DIFFERENT model ⇒ TWO independent rows
// (was ONE at the 4-col grain); each model's blend is untouched by the other.
func TestRecordServe_ModelGrain_DistinctRowsPerModel(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	// Same (node, chat/small/trivial), two different models.
	if err := s.RecordServe(ctx, "nodeA", "chat", "small", "trivial", "gpt-4o", 100, 3); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordServe(ctx, "nodeA", "chat", "small", "trivial", "claude-haiku-4-5", 500, 3); err != nil {
		t.Fatal(err)
	}

	// TWO rows for the SAME cohort, one per model.
	var rows int
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM node_cohort_latency_stats
		 WHERE node_id='nodeA' AND feature_category='chat' AND input_token_range='small' AND complexity_bucket='trivial'`).Scan(&rows)
	if rows != 2 {
		t.Fatalf("same cohort, two models must yield 2 rows (model grain), got %d", rows)
	}

	// A second serve on the gpt-4o row blends it; the claude row is UNTOUCHED (independent grain).
	if err := s.RecordServe(ctx, "nodeA", "chat", "small", "trivial", "gpt-4o", 200, 4); err != nil {
		t.Fatal(err)
	}
	if e, _, n := rowOf(t, pool, "nodeA", "chat", "small", "trivial", "gpt-4o"); math.Abs(e-120) > 1e-9 || n != 2 {
		t.Fatalf("gpt-4o blend at model grain: ewma=%.3f n=%d, want 120/2", e, n)
	}
	if e, _, n := rowOf(t, pool, "nodeA", "chat", "small", "trivial", "claude-haiku-4-5"); e != 500 || n != 1 {
		t.Fatalf("claude row mutated by the gpt-4o blend (grains not independent): ewma=%.3f n=%d, want 500/1", e, n)
	}
}

// A nil-pool store is a no-op (never panics, never errors).
func TestRecordServe_NilStoreNoOp(t *testing.T) {
	var s *Store
	if err := s.RecordServe(context.Background(), "n", "f", "i", "c", "m", 1, 1); err != nil {
		t.Fatalf("nil store must no-op, got %v", err)
	}
	if err := (&Store{}).RecordServe(context.Background(), "n", "f", "i", "c", "m", 1, 1); err != nil {
		t.Fatalf("empty store must no-op, got %v", err)
	}
}
