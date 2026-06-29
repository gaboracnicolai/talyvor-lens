package benchprobe

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Real-PG proofs for the proof-of-eval-contribution draw-time defenses:
//   - AUTHOR-EXCLUSION (proof 1): an item is never drawn for the author's own node, nor for a node in
//     the author's owner-linkage fingerprint-linked set (a same-card sister workspace); a genuinely
//     unlinked node CAN draw it.
//   - ANTI-FARMING (proof 3): a duplicate content_hash is rejected at contribute; a pending item is
//     never drawn (and so never accumulates probes / earns).

func drawHarness(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG draw-exclusion test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS benchmark_probes`,
		`DROP TABLE IF EXISTS benchmark_eval_items`,
		`DROP TABLE IF EXISTS inference_nodes`,
		`DROP TABLE IF EXISTS workspace_card_fingerprints`,
		`CREATE TABLE benchmark_eval_items (id TEXT PRIMARY KEY, input TEXT NOT NULL, expected_output TEXT NOT NULL,
			eval_method TEXT NOT NULL DEFAULT 'exact', pass_threshold DOUBLE PRECISION NOT NULL DEFAULT 1.0,
			active BOOLEAN NOT NULL DEFAULT TRUE, content_hash TEXT, status TEXT NOT NULL DEFAULT 'active',
			author_workspace_id TEXT, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE UNIQUE INDEX idx_eval_items_content_hash ON benchmark_eval_items (content_hash) WHERE content_hash IS NOT NULL`,
		`CREATE TABLE benchmark_probes (id TEXT PRIMARY KEY, node_id TEXT NOT NULL, item_id TEXT NOT NULL,
			request_id TEXT, served_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), score DOUBLE PRECISION NOT NULL DEFAULT 0,
			UNIQUE (node_id, item_id))`,
		`CREATE TABLE inference_nodes (id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, url TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT 'vllm')`,
		`CREATE TABLE workspace_card_fingerprints (workspace_id TEXT NOT NULL, fingerprint_hash TEXT NOT NULL, PRIMARY KEY (workspace_id, fingerprint_hash))`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return NewStore(pool), pool
}

func regNode(t *testing.T, pool *pgxpool.Pool, node, ws string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO inference_nodes (id, workspace_id) VALUES ($1,$2)`, node, ws); err != nil {
		t.Fatal(err)
	}
}

func linkCard(t *testing.T, pool *pgxpool.Pool, ws, fp string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO workspace_card_fingerprints (workspace_id, fingerprint_hash) VALUES ($1,$2)`, ws, fp); err != nil {
		t.Fatal(err)
	}
}

func drawnID(t *testing.T, s *Store, node string) string {
	t.Helper()
	it, err := s.DrawItem(context.Background(), node)
	if err != nil {
		t.Fatalf("draw: %v", err)
	}
	if it == nil {
		return ""
	}
	return it.ID
}

// (proof 1) Author-exclusion: same-workspace AND fingerprint-linked nodes never draw the author's item;
// a genuinely unlinked node does.
func TestDraw_AuthorAndLinkedExclusion_Integration(t *testing.T) {
	s, pool := drawHarness(t)
	ctx := context.Background()

	// Author wsA; wsB shares card fingerprint "fp-1" with wsA (a sister workspace); wsC is unlinked.
	linkCard(t, pool, "wsA", "fp-1")
	linkCard(t, pool, "wsB", "fp-1")
	regNode(t, pool, "nodeA", "wsA")
	regNode(t, pool, "nodeB", "wsB")
	regNode(t, pool, "nodeC", "wsC")

	// An item authored by wsA, validated active.
	if _, err := pool.Exec(ctx,
		`INSERT INTO benchmark_eval_items (id, input, expected_output, status, author_workspace_id, content_hash)
		 VALUES ('itemA','in','out','active','wsA','h1')`); err != nil {
		t.Fatal(err)
	}

	if got := drawnID(t, s, "nodeA"); got == "itemA" {
		t.Error("author's own node must NEVER draw the author's item")
	}
	if got := drawnID(t, s, "nodeB"); got == "itemA" {
		t.Error("a fingerprint-LINKED sister workspace's node must never draw the author's item")
	}
	if got := drawnID(t, s, "nodeC"); got != "itemA" {
		t.Errorf("an UNLINKED node must be able to draw the item, got %q", got)
	}
}

// (proof 3) Anti-farming: duplicate content_hash rejected at contribute; a pending item is never drawn.
func TestDraw_DedupAndPending_Integration(t *testing.T) {
	s, pool := drawHarness(t)
	ctx := context.Background()
	regNode(t, pool, "nodeC", "wsC") // an unlinked grader

	// Contribute an authored item (lands pending). It must NOT be drawable.
	if err := s.ContributeItem(ctx, EvalItem{ID: "c1", Input: "dup-input", ExpectedOutput: "o", AuthorWorkspaceID: "wsA"}); err != nil {
		t.Fatalf("contribute: %v", err)
	}
	if got := drawnID(t, s, "nodeC"); got == "c1" {
		t.Error("a PENDING contributed item must never be drawn (not yet validated)")
	}

	// A second item with the SAME input (same content_hash) is rejected — exact dedup.
	if err := s.ContributeItem(ctx, EvalItem{ID: "c2", Input: "dup-input", ExpectedOutput: "o2", AuthorWorkspaceID: "wsA"}); !errors.Is(err, ErrDuplicateItem) {
		t.Fatalf("duplicate content_hash must be rejected with ErrDuplicateItem, got %v", err)
	}

	// After validation, the (unique) item becomes drawable by the unlinked node.
	if err := s.ValidateItem(ctx, "c1"); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := drawnID(t, s, "nodeC"); got != "c1" {
		t.Errorf("a validated item must be drawable by an unlinked node, got %q", got)
	}
}
