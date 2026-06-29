package benchprobe

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/cohort"
)

// Real-PG proofs for PR-2 cohort tagging:
//   - a seeded item stores the cohort it was given (which the tool derives via cohort.DeriveInputCohort);
//   - feature declared vs NULL; node-blind (BuildProbeRequest carries Input only); backfill.

func cohortHarness(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG cohort-tag test")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "lens_cohorttag_test"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP SCHEMA IF EXISTS lens_cohorttag_test CASCADE`,
		`CREATE SCHEMA lens_cohorttag_test`,
		`CREATE TABLE benchmark_eval_items (id TEXT PRIMARY KEY, input TEXT NOT NULL, expected_output TEXT NOT NULL,
			eval_method TEXT NOT NULL DEFAULT 'exact', pass_threshold DOUBLE PRECISION NOT NULL DEFAULT 1.0,
			active BOOLEAN NOT NULL DEFAULT TRUE, content_hash TEXT, status TEXT NOT NULL DEFAULT 'active',
			author_workspace_id TEXT, feature_category TEXT, input_token_range TEXT, complexity_bucket TEXT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return NewStore(pool), pool
}

func cohortOf(t *testing.T, pool *pgxpool.Pool, id string) (feature, inRange, complexity *string) {
	t.Helper()
	_ = pool.QueryRow(context.Background(),
		`SELECT feature_category, input_token_range, complexity_bucket FROM benchmark_eval_items WHERE id=$1`, id).
		Scan(&feature, &inRange, &complexity)
	return
}

// (proof 1) A seeded item stores the SAME cohort cohort.DeriveInputCohort returns for its input.
func TestSeed_StoresDerivedCohort_Integration(t *testing.T) {
	s, pool := cohortHarness(t)
	ctx := context.Background()
	input := "Write a Go function that reverses a linked list and explain its complexity."
	ir, cb := cohort.DeriveInputCohort(input) // the tool computes this; the store persists it

	if err := s.SeedItem(ctx, EvalItem{
		ID: "i1", Input: input, ExpectedOutput: "out",
		FeatureCategory: "code", InputTokenRange: ir, ComplexityBucket: cb,
	}); err != nil {
		t.Fatal(err)
	}
	feature, gotIR, gotCB := cohortOf(t, pool, "i1")
	if gotIR == nil || *gotIR != ir || gotCB == nil || *gotCB != cb {
		t.Fatalf("stored cohort != derived: stored=(%v,%v) derived=(%q,%q)", gotIR, gotCB, ir, cb)
	}
	if feature == nil || *feature != "code" {
		t.Fatalf("declared feature_category not stored: %v", feature)
	}
}

// (proof 2) Feature declared vs NULL: an item seeded WITHOUT a feature has feature_category NULL (untagged).
func TestSeed_FeatureDeclaredOrNull_Integration(t *testing.T) {
	s, pool := cohortHarness(t)
	ctx := context.Background()
	ir, cb := cohort.DeriveInputCohort("hi")
	if err := s.SeedItem(ctx, EvalItem{ID: "nofeat", Input: "hi", ExpectedOutput: "o", InputTokenRange: ir, ComplexityBucket: cb}); err != nil {
		t.Fatal(err)
	}
	feature, gotIR, _ := cohortOf(t, pool, "nofeat")
	if feature != nil {
		t.Fatalf("feature_category must be NULL when undeclared, got %q", *feature)
	}
	if gotIR == nil { // the derived dim is still set
		t.Fatal("input_token_range must be set even when feature is NULL")
	}
}

// (proof 3) Node-blind: the ON-WIRE probe payload for a fully-tagged item carries Input only — no
// cohort, no expected_output. The cohort tag is verifier-private exactly like the ground truth.
func TestCohort_NodeBlind_PayloadCarriesInputOnly(t *testing.T) {
	item := EvalItem{
		ID: "i", Input: "the input", ExpectedOutput: "SECRET-TRUTH",
		FeatureCategory: "FEATCAT", InputTokenRange: "RANGEVAL", ComplexityBucket: "CPLXVAL",
	}
	wire, err := json.Marshal(BuildProbeRequest("m", item))
	if err != nil {
		t.Fatal(err)
	}
	s := string(wire)
	if !strings.Contains(s, "the input") {
		t.Fatalf("probe must carry the input; wire=%s", s)
	}
	for _, leak := range []string{"SECRET-TRUTH", "FEATCAT", "RANGEVAL", "CPLXVAL", "feature_category", "complexity_bucket", "expected_output"} {
		if strings.Contains(s, leak) {
			t.Errorf("probe payload leaks %q — cohort/ground-truth must stay verifier-private; wire=%s", leak, s)
		}
	}
}

// (proof 5) Backfill: an item seeded with NO cohort is found by ItemsMissingCohort and tagged with the
// two derived dims; feature_category stays NULL.
func TestBackfill_DerivesTwoDims_Integration(t *testing.T) {
	s, pool := cohortHarness(t)
	ctx := context.Background()
	// Seed an untagged row directly (no cohort fields set ⇒ NULL).
	if err := s.SeedItem(ctx, EvalItem{ID: "legacy", Input: "explain TCP slow start in detail", ExpectedOutput: "o"}); err != nil {
		t.Fatal(err)
	}
	if _, ir, _ := cohortOf(t, pool, "legacy"); ir != nil {
		t.Fatalf("precondition: legacy row must start untagged, got %v", ir)
	}
	todo, err := s.ItemsMissingCohort(ctx, 100)
	if err != nil || len(todo) != 1 || todo[0].ID != "legacy" {
		t.Fatalf("ItemsMissingCohort must return the legacy row: %v err=%v", todo, err)
	}
	ir, cb := cohort.DeriveInputCohort(todo[0].Input)
	if err := s.BackfillCohort(ctx, "legacy", ir, cb); err != nil {
		t.Fatal(err)
	}
	feature, gotIR, gotCB := cohortOf(t, pool, "legacy")
	if gotIR == nil || *gotIR != ir || gotCB == nil || *gotCB != cb {
		t.Fatalf("backfill did not set the derived dims: (%v,%v) want (%q,%q)", gotIR, gotCB, ir, cb)
	}
	if feature != nil {
		t.Fatalf("feature_category must stay NULL after backfill (not derivable), got %q", *feature)
	}
}
