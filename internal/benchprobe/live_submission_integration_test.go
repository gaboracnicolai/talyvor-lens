package benchprobe_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/benchprobe"
)

// The LIVE eval-submission surface (the endpoint's write layer, NOT the seed CLI): SubmitLiveEval lands a
// contributor's eval ACTIVE + author-stamped + content-deduped; AttestCorrectness records other workspaces'
// correctness judgments and rejects self-attestation / unknown items.
func liveSubmitHarness(t *testing.T) (*pgxpool.Pool, *benchprobe.Store) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping live-submission test")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "lens_benchprobe_livesubmit_test"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP SCHEMA IF EXISTS lens_benchprobe_livesubmit_test CASCADE`,
		`CREATE SCHEMA lens_benchprobe_livesubmit_test`,
		`CREATE TABLE benchmark_eval_items (id TEXT PRIMARY KEY, input TEXT NOT NULL, expected_output TEXT NOT NULL,
			eval_method TEXT NOT NULL DEFAULT 'exact', pass_threshold DOUBLE PRECISION NOT NULL DEFAULT 1.0,
			active BOOLEAN NOT NULL DEFAULT FALSE, content_hash TEXT, status TEXT NOT NULL DEFAULT 'pending',
			author_workspace_id TEXT, feature_category TEXT, input_token_range TEXT, complexity_bucket TEXT)`,
		`CREATE UNIQUE INDEX idx_eval_items_content_hash ON benchmark_eval_items (content_hash)`,
		`CREATE TABLE eval_correctness_attestations (item_id TEXT NOT NULL, attester_workspace_id TEXT NOT NULL,
			agrees BOOLEAN NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (item_id, attester_workspace_id))`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool, benchprobe.NewStore(pool)
}

func TestSubmitLiveEval_LandsActiveDeduped(t *testing.T) {
	pool, store := liveSubmitHarness(t)
	ctx := context.Background()

	id, err := store.SubmitLiveEval(ctx, benchprobe.EvalItem{
		Input: "What is the capital of France?", ExpectedOutput: "Paris", AuthorWorkspaceID: "wsA",
	})
	if err != nil {
		t.Fatalf("SubmitLiveEval: %v", err)
	}
	if id == "" {
		t.Fatal("SubmitLiveEval must return a generated id")
	}
	var active bool
	var status, author string
	if err := pool.QueryRow(ctx, `SELECT active, status, author_workspace_id FROM benchmark_eval_items WHERE id=$1`, id).
		Scan(&active, &status, &author); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if !active || status != "active" || author != "wsA" {
		t.Fatalf("live eval must be active/active/wsA, got active=%v status=%q author=%q (the LIVE surface activates directly; earning is consensus-gated)", active, status, author)
	}

	// Same input (content_hash) → the exact-dedup anti-farm reject.
	if _, err := store.SubmitLiveEval(ctx, benchprobe.EvalItem{
		Input: "What is the capital of France?", ExpectedOutput: "Paris (again)", AuthorWorkspaceID: "wsB",
	}); !errors.Is(err, benchprobe.ErrDuplicateItem) {
		t.Fatalf("duplicate input must return ErrDuplicateItem, got %v", err)
	}

	// Missing author / input rejected.
	if _, err := store.SubmitLiveEval(ctx, benchprobe.EvalItem{Input: "x", ExpectedOutput: "y"}); err == nil {
		t.Fatal("SubmitLiveEval without author must error")
	}
	if _, err := store.SubmitLiveEval(ctx, benchprobe.EvalItem{ExpectedOutput: "y", AuthorWorkspaceID: "wsA"}); err == nil {
		t.Fatal("SubmitLiveEval without input must error")
	}
}

func TestAttestCorrectness_SelfRejectUnknownUpsert(t *testing.T) {
	pool, store := liveSubmitHarness(t)
	ctx := context.Background()
	id, err := store.SubmitLiveEval(ctx, benchprobe.EvalItem{Input: "2+2?", ExpectedOutput: "4", AuthorWorkspaceID: "wsA"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// The author cannot attest its own item.
	if err := store.AttestCorrectness(ctx, id, "wsA", true); !errors.Is(err, benchprobe.ErrSelfAttestation) {
		t.Fatalf("self-attestation must return ErrSelfAttestation, got %v", err)
	}
	// Unknown item.
	if err := store.AttestCorrectness(ctx, "no-such-item", "wsB", true); !errors.Is(err, benchprobe.ErrUnknownItem) {
		t.Fatalf("unknown item must return ErrUnknownItem, got %v", err)
	}
	// Another workspace attests → ok; re-attest upserts (flips) the SAME single vote (no inflation).
	if err := store.AttestCorrectness(ctx, id, "wsB", true); err != nil {
		t.Fatalf("attest by other: %v", err)
	}
	if err := store.AttestCorrectness(ctx, id, "wsB", false); err != nil {
		t.Fatalf("re-attest by other: %v", err)
	}
	var rows int
	var agrees bool
	_ = pool.QueryRow(ctx, `SELECT count(*), bool_or(agrees) FROM eval_correctness_attestations WHERE item_id=$1 AND attester_workspace_id='wsB'`, id).
		Scan(&rows, &agrees)
	if rows != 1 || agrees {
		t.Fatalf("re-attest must UPSERT one row to the new value, got rows=%d agrees=%v", rows, agrees)
	}
}
