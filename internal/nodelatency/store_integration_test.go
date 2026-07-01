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
		complexity_bucket TEXT NOT NULL, latency_ewma DOUBLE PRECISION NOT NULL DEFAULT 0,
		cost_weight_accum DOUBLE PRECISION NOT NULL DEFAULT 0, sample_count BIGINT NOT NULL DEFAULT 0,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (node_id, feature_category, input_token_range, complexity_bucket))`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `TRUNCATE node_cohort_latency_stats`); err != nil {
		t.Fatal(err)
	}
	return &Store{db: pool}, pool
}

func rowOf(t *testing.T, pool *pgxpool.Pool, node, feat, itr, cb string) (ewma, cost float64, n int64) {
	t.Helper()
	_ = pool.QueryRow(context.Background(),
		`SELECT latency_ewma, cost_weight_accum, sample_count FROM node_cohort_latency_stats
		 WHERE node_id=$1 AND feature_category=$2 AND input_token_range=$3 AND complexity_bucket=$4`,
		node, feat, itr, cb).Scan(&ewma, &cost, &n)
	return
}

// (proof 1) Capture correctness + the α=0.2 EWMA blend: first serve seeds raw; a second on the SAME
// (node,cohort) blends latency via 0.2 and accumulates cost + count. A different cohort is a separate row.
func TestRecordServe_EWMABlendAndAccumulation(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	// First serve: seeds latency_ewma = 100, cost_weight_accum = 3, sample_count = 1.
	if err := s.RecordServe(ctx, "nodeA", "chat", "small", "trivial", 100, 3); err != nil {
		t.Fatal(err)
	}
	ewma, cost, n := rowOf(t, pool, "nodeA", "chat", "small", "trivial")
	if ewma != 100 || cost != 3 || n != 1 {
		t.Fatalf("seed: got ewma=%.3f cost=%.1f n=%d, want 100/3/1", ewma, cost, n)
	}

	// Second serve on the SAME cohort: latency 200, cost 4 → ewma = 100*0.8 + 200*0.2 = 120; cost 3+4=7; n=2.
	if err := s.RecordServe(ctx, "nodeA", "chat", "small", "trivial", 200, 4); err != nil {
		t.Fatal(err)
	}
	ewma, cost, n = rowOf(t, pool, "nodeA", "chat", "small", "trivial")
	if math.Abs(ewma-120) > 1e-9 || cost != 7 || n != 2 {
		t.Fatalf("blend: got ewma=%.6f cost=%.1f n=%d, want 120/7/2 (100*0.8+200*0.2)", ewma, cost, n)
	}

	// A DIFFERENT cohort (complexity_bucket) is a distinct row — untouched by the above.
	if err := s.RecordServe(ctx, "nodeA", "chat", "small", "hard", 500, 5); err != nil {
		t.Fatal(err)
	}
	ewma, cost, n = rowOf(t, pool, "nodeA", "chat", "small", "hard")
	if ewma != 500 || cost != 5 || n != 1 {
		t.Fatalf("distinct cohort: got ewma=%.3f cost=%.1f n=%d, want 500/5/1", ewma, cost, n)
	}
	// The original cohort row is unchanged by the different-cohort write.
	if ewma2, _, n2 := rowOf(t, pool, "nodeA", "chat", "small", "trivial"); ewma2 != 120 || n2 != 2 {
		t.Fatalf("original cohort mutated by a different-cohort write: ewma=%.3f n=%d", ewma2, n2)
	}
}

// A nil-pool store is a no-op (never panics, never errors).
func TestRecordServe_NilStoreNoOp(t *testing.T) {
	var s *Store
	if err := s.RecordServe(context.Background(), "n", "f", "i", "c", 1, 1); err != nil {
		t.Fatalf("nil store must no-op, got %v", err)
	}
	if err := (&Store{}).RecordServe(context.Background(), "n", "f", "i", "c", 1, 1); err != nil {
		t.Fatalf("empty store must no-op, got %v", err)
	}
}
