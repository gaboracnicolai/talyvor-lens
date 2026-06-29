package benchprobe_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/benchprobe"
)

// Proof-of-benchmark MEASUREMENT (PR-A, delivery FAKED). Real-PG; private schema so a parallel
// package can't collide on the 0068 table names.

type fakeDelivery struct {
	answer  string
	called  int
	lastReq benchprobe.ProbeRequest
}

func (f *fakeDelivery) Deliver(_ context.Context, _, _ string, req benchprobe.ProbeRequest) (string, error) {
	f.called++
	f.lastReq = req
	return f.answer, nil
}

func harness(t *testing.T) (*pgxpool.Pool, *benchprobe.Store) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG benchprobe test")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "lens_benchprobe_test"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP SCHEMA IF EXISTS lens_benchprobe_test CASCADE`,
		`CREATE SCHEMA lens_benchprobe_test`,
		`CREATE TABLE benchmark_eval_items (id TEXT PRIMARY KEY, input TEXT NOT NULL, expected_output TEXT NOT NULL,
			eval_method TEXT NOT NULL DEFAULT 'exact', pass_threshold DOUBLE PRECISION NOT NULL DEFAULT 1.0,
			active BOOLEAN NOT NULL DEFAULT TRUE, content_hash TEXT, status TEXT NOT NULL DEFAULT 'active',
			author_workspace_id TEXT, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), retired_at TIMESTAMPTZ)`,
		`CREATE TABLE benchmark_node_scores (node_id TEXT NOT NULL, model TEXT NOT NULL, score DOUBLE PRECISION NOT NULL DEFAULT 0,
			sample_count INTEGER NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (node_id, model))`,
		`CREATE TABLE benchmark_probes (id TEXT PRIMARY KEY, node_id TEXT NOT NULL, item_id TEXT NOT NULL, request_id TEXT,
			served_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), score DOUBLE PRECISION NOT NULL DEFAULT 0, UNIQUE (node_id, item_id))`,
		// Referenced by the author-exclusion DrawItem predicate (P-o-I instance 1); empty here (no authored items).
		`CREATE TABLE inference_nodes (id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, url TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT 'vllm')`,
		`CREATE TABLE workspace_card_fingerprints (workspace_id TEXT NOT NULL, fingerprint_hash TEXT NOT NULL, PRIMARY KEY (workspace_id, fingerprint_hash))`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool, benchprobe.NewStore(pool)
}

func probeCount(t *testing.T, pool *pgxpool.Pool, node string) int {
	t.Helper()
	var n int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM benchmark_probes WHERE node_id=$1`, node).Scan(&n)
	return n
}

// (1) Unpredictable draw + NEVER-REUSED: an item probed to a node is never redrawn; the pool exhausts.
func TestDraw_NeverReused_Integration(t *testing.T) {
	_, store := harness(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		if err := store.SeedItem(ctx, benchprobe.EvalItem{ID: id, Input: "in-" + id, ExpectedOutput: "out-" + id, EvalMethod: "exact"}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.DrawItem(ctx, "nodeX")
	if err != nil || first == nil {
		t.Fatalf("first draw: %v / %v", first, err)
	}
	if err := store.RecordProbe(ctx, benchprobe.Probe{NodeID: "nodeX", ItemID: first.ID}); err != nil {
		t.Fatal(err)
	}
	second, err := store.DrawItem(ctx, "nodeX")
	if err != nil || second == nil {
		t.Fatalf("second draw: %v / %v", second, err)
	}
	if second.ID == first.ID {
		t.Fatalf("never-reuse violated: item %q drawn twice for nodeX", first.ID)
	}
	if err := store.RecordProbe(ctx, benchprobe.Probe{NodeID: "nodeX", ItemID: second.ID}); err != nil {
		t.Fatal(err)
	}
	// Both items probed → pool exhausted for nodeX → nil (no-op, not error).
	third, err := store.DrawItem(ctx, "nodeX")
	if err != nil {
		t.Fatal(err)
	}
	if third != nil {
		t.Errorf("expected exhausted pool (nil) for nodeX, got %q", third.ID)
	}
	// A DIFFERENT node can still draw both (per-node never-reuse, not global).
	if d, _ := store.DrawItem(ctx, "nodeY"); d == nil {
		t.Error("nodeY should still have undrawn items")
	}
}

// (2) NODE-BLIND payload: the constructed probe carries the input but NEVER the expected output.
func TestPayload_NodeBlind_NoGroundTruth(t *testing.T) {
	item := benchprobe.EvalItem{ID: "i1", Input: "What is 2+2?", ExpectedOutput: "SECRET-GROUND-TRUTH-4", EvalMethod: "exact"}
	req := benchprobe.BuildProbeRequest("trial-mock", item)
	if req.Input != item.Input {
		t.Errorf("probe input %q, want %q", req.Input, item.Input)
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "SECRET-GROUND-TRUTH-4") || strings.Contains(string(b), "expected") {
		t.Fatalf("node-blind VIOLATED: probe payload leaks ground truth: %s", b)
	}
}

// (3) Scoring correctness via the scheduler (faked delivery): correct→1.0, wrong→0; covers exact/contains/regex/json_schema.
func TestScheduler_ScoringCorrectness_Integration(t *testing.T) {
	pool, store := harness(t)
	ctx := context.Background()
	cases := []struct {
		id, method, expected, answer string
		want                         float64
	}{
		{"exact-ok", "exact", "4", "4", 1.0},
		{"exact-no", "exact", "4", "5", 0.0},
		{"contains-ok", "contains", "Paris", "The capital is Paris.", 1.0},
		{"regex-ok", "regex", `^\d{3}$`, "404", 1.0},
		{"json-ok", "json_schema", "", `{"a":1}`, 1.0},
		{"json-no", "json_schema", "", "not json", 0.0},
	}
	for _, c := range cases {
		if err := store.SeedItem(ctx, benchprobe.EvalItem{ID: c.id, Input: "q", ExpectedOutput: c.expected, EvalMethod: c.method}); err != nil {
			t.Fatal(err)
		}
		fd := &fakeDelivery{answer: c.answer}
		sched := benchprobe.NewScheduler(store, fd, func() bool { return true })
		// Draw is unpredictable, so retire all but this item to force it (active flag).
		if _, err := pool.Exec(ctx, `UPDATE benchmark_eval_items SET active=(id=$1)`, c.id); err != nil {
			t.Fatal(err)
		}
		node := "node-" + c.id
		if err := sched.RunOnceForNode(ctx, node, "m"); err != nil {
			t.Fatalf("%s: RunOnce: %v", c.id, err)
		}
		got, n, _ := store.NodeScore(ctx, node, "m")
		if n != 1 || got < c.want-1e-9 || got > c.want+1e-9 {
			t.Errorf("%s: score=%v (n=%d), want %v", c.id, got, n, c.want)
		}
	}
}

// (5) FLAG-OFF byte-identical: no draw, no delivery, no write.
func TestScheduler_FlagOff_NoOp_Integration(t *testing.T) {
	pool, store := harness(t)
	ctx := context.Background()
	if err := store.SeedItem(ctx, benchprobe.EvalItem{ID: "i", Input: "q", ExpectedOutput: "a", EvalMethod: "exact"}); err != nil {
		t.Fatal(err)
	}
	fd := &fakeDelivery{answer: "a"}
	sched := benchprobe.NewScheduler(store, fd, func() bool { return false }) // OFF
	if err := sched.RunOnceForNode(ctx, "nodeOff", "m"); err != nil {
		t.Fatal(err)
	}
	if fd.called != 0 {
		t.Errorf("flag-off delivered %d probes, want 0", fd.called)
	}
	if probeCount(t, pool, "nodeOff") != 0 {
		t.Error("flag-off recorded a probe row, want none")
	}
	if _, n, _ := store.NodeScore(ctx, "nodeOff", "m"); n != 0 {
		t.Error("flag-off wrote a node score, want none")
	}
}
