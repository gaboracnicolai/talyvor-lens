package routingpredict

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Real-PG proofs for the routing-prediction substrate (PR-1):
//   - submit + attribute; dedup (one live model per cohort); lifecycle (validate/retire); capability gate.

func predHarness(t *testing.T, enabled func() bool) (*Store, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG routing-prediction test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS routing_predictions`,
		`CREATE TABLE routing_predictions (id TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
			workspace_id TEXT NOT NULL, feature_category TEXT NOT NULL, input_token_range TEXT NOT NULL,
			complexity_bucket TEXT NOT NULL DEFAULT '', model TEXT NOT NULL, provider TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'pending', created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE UNIQUE INDEX idx_routing_predictions_live ON routing_predictions
			(workspace_id, feature_category, input_token_range, complexity_bucket) WHERE status IN ('pending','active')`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return NewStore(pool, enabled), pool
}

func on() func() bool  { return func() bool { return true } }
func off() func() bool { return func() bool { return false } }

// (proof 1) Submit + attribute: a pending row stamped with the workspace.
func TestSubmit_AttributesWorkspace_Integration(t *testing.T) {
	s, _ := predHarness(t, on())
	ctx := context.Background()
	id, err := s.SubmitPrediction(ctx, Prediction{
		WorkspaceID: "wsA", FeatureCategory: "summarize", InputTokenRange: "0-500",
		ComplexityBucket: "simple", Model: "gpt-4o-mini", Provider: "openai",
	})
	if err != nil || id == "" {
		t.Fatalf("submit must succeed and return an id: id=%q err=%v", id, err)
	}
	got, _ := s.GetPrediction(ctx, id)
	if got == nil || got.WorkspaceID != "wsA" || got.Model != "gpt-4o-mini" || got.Status != "pending" {
		t.Fatalf("read-back mismatch: %+v", got)
	}
}

// (proof 2) Dedup: a second LIVE prediction for the same (workspace, cohort) is rejected — even with a
// DIFFERENT model (one live model per cohort). A different cohort, or a different workspace, is allowed.
func TestSubmit_OneLivePerCohort_Integration(t *testing.T) {
	s, _ := predHarness(t, on())
	ctx := context.Background()
	base := Prediction{WorkspaceID: "wsA", FeatureCategory: "summarize", InputTokenRange: "0-500", ComplexityBucket: "simple", Model: "gpt-4o-mini"}
	if _, err := s.SubmitPrediction(ctx, base); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	// Same (workspace, cohort), DIFFERENT model → still rejected (no hedging).
	dup := base
	dup.Model = "claude-haiku-4-5"
	if _, err := s.SubmitPrediction(ctx, dup); !errors.Is(err, ErrDuplicatePrediction) {
		t.Fatalf("same (workspace,cohort) with a different model must be ErrDuplicatePrediction, got %v", err)
	}
	// Different cohort (complexity differs) → allowed.
	other := base
	other.ComplexityBucket = "complex"
	if _, err := s.SubmitPrediction(ctx, other); err != nil {
		t.Fatalf("a different cohort must be allowed: %v", err)
	}
	// Different workspace, same cohort → allowed.
	ws2 := base
	ws2.WorkspaceID = "wsB"
	if _, err := s.SubmitPrediction(ctx, ws2); err != nil {
		t.Fatalf("a different workspace must be allowed: %v", err)
	}
}

// (proof 3) Lifecycle: validate pending→active; retire frees the dedup slot so a NEW model can be asserted.
func TestLifecycle_ValidateRetireFreesSlot_Integration(t *testing.T) {
	s, _ := predHarness(t, on())
	ctx := context.Background()
	p := Prediction{WorkspaceID: "wsA", FeatureCategory: "code", InputTokenRange: "500-2000", Model: "gpt-4o"}
	id, err := s.SubmitPrediction(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ValidatePrediction(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetPrediction(ctx, id); got.Status != "active" {
		t.Fatalf("status after validate = %q, want active", got.Status)
	}
	// Active still blocks a duplicate.
	if _, err := s.SubmitPrediction(ctx, p); !errors.Is(err, ErrDuplicatePrediction) {
		t.Fatalf("active prediction must still block duplicates, got %v", err)
	}
	// Retire frees the slot → a new model for the same cohort is now allowed.
	if err := s.RetirePrediction(ctx, id); err != nil {
		t.Fatal(err)
	}
	p2 := p
	p2.Model = "claude-sonnet-4-6"
	if _, err := s.SubmitPrediction(ctx, p2); err != nil {
		t.Fatalf("after retire, a new model for the cohort must be allowed: %v", err)
	}
}

// (proof 4) Capability gate: with the flag OFF, submission is refused and the table stays empty; ON, it works.
func TestSubmit_CapabilityFlag_Integration(t *testing.T) {
	s, pool := predHarness(t, off())
	ctx := context.Background()
	p := Prediction{WorkspaceID: "wsA", FeatureCategory: "summarize", InputTokenRange: "0-500", Model: "gpt-4o-mini"}
	if _, err := s.SubmitPrediction(ctx, p); !errors.Is(err, ErrSubmissionDisabled) {
		t.Fatalf("flag off must refuse with ErrSubmissionDisabled, got %v", err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM routing_predictions`).Scan(&n)
	if n != 0 {
		t.Fatalf("flag off: table must stay empty, got %d rows", n)
	}
	// Flip on (same pool) → submission works.
	live := NewStore(pool, on())
	if _, err := live.SubmitPrediction(ctx, p); err != nil {
		t.Fatalf("flag on must allow submission: %v", err)
	}
}

// Validation: a bad complexity bucket is rejected before any write.
func TestSubmit_RejectsBadComplexity_Integration(t *testing.T) {
	s, _ := predHarness(t, on())
	_, err := s.SubmitPrediction(context.Background(), Prediction{
		WorkspaceID: "wsA", FeatureCategory: "summarize", InputTokenRange: "0-500", ComplexityBucket: "BOGUS", Model: "m",
	})
	if !errors.Is(err, ErrInvalidPrediction) {
		t.Fatalf("bad complexity must be ErrInvalidPrediction, got %v", err)
	}
}
