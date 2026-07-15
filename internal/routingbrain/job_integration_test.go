package routingbrain

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/keel"
	"github.com/talyvor/lens/internal/modelcapability"
)

// fakeWorkspaces is a canned workspace enumeration (the job's workspaceSource).
type fakeWorkspaces struct{ ws []WorkspaceModels }

func (f fakeWorkspaces) BrainWorkspaces() []WorkspaceModels { return f.ws }

// seedH2AndKeel creates + seeds model_capability_observations (H2) and keel_findings
// in the test's private schema, so the job reads them through the REAL stores. It
// seeds a degrader A, a flat B, and a high-but-Keel-ADVERSE C.
func seedH2AndKeel(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS model_capability_observations`,
		`DROP TABLE IF EXISTS keel_findings`,
		`CREATE TABLE model_capability_observations (
			id BIGSERIAL PRIMARY KEY, model TEXT NOT NULL, provider TEXT NOT NULL DEFAULT '',
			difficulty INTEGER NOT NULL, quality DOUBLE PRECISION NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE keel_findings (
			workspace_id TEXT NOT NULL, unit TEXT NOT NULL, window_bucket BIGINT NOT NULL,
			deviation_sigma DOUBLE PRECISION NOT NULL, attribution TEXT NOT NULL,
			cohort_n INTEGER NOT NULL DEFAULT 0, identity_key TEXT UNIQUE, metrics JSONB,
			mode TEXT NOT NULL DEFAULT 'ordinary', first_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("seed schema: %v", err)
		}
	}
	mc := modelcapability.NewStore(pool)
	for d := 0; d <= modelcapability.MaxDifficulty; d++ {
		for i := 0; i < 3; i++ { // multi-sample per difficulty
			if err := mc.Record(ctx, "A", "openai", d, 0.90-0.10*float64(d)); err != nil { // degrader
				t.Fatal(err)
			}
			if err := mc.Record(ctx, "B", "openai", d, 0.60); err != nil { // holder
				t.Fatal(err)
			}
			if err := mc.Record(ctx, "C", "openai", d, 0.95); err != nil { // best-looking, but ADVERSE
				t.Fatal(err)
			}
		}
	}
	// Keel: ws1's model C drifted idiosyncratically WORSE than its cohort (sigma < 0) → adverse.
	if _, err := pool.Exec(ctx, `INSERT INTO keel_findings
		(workspace_id, unit, window_bucket, deviation_sigma, attribution, cohort_n, identity_key, mode)
		VALUES ('ws1','openai/C',2,-3.0,'idiosyncratic',5,'k1','ordinary')`); err != nil {
		t.Fatalf("seed keel finding: %v", err)
	}
}

// TestJob_RunOnce_ComputesFromRealH2AndKeel_RealPG — the offline job reads the REAL
// H2 capability curves (modelcapability.Fit) and the REAL Keel findings
// (keel.Reader), computes recommendations, and upserts them. Proves the brain
// consumes both organs: the Keel-adverse model C is excluded, and the pick is
// curve-driven (A wins the low tiers, B wins the high tiers where A degrades).
func TestJob_RunOnce_ComputesFromRealH2AndKeel_RealPG(t *testing.T) {
	pool := rbTestPool(t)
	ctx := context.Background()
	seedH2AndKeel(t, ctx, pool)

	store := NewStore(pool)
	job := NewJob(
		modelcapability.NewStore(pool),                                  // curveSource (H2)
		keel.NewReader(pool),                                            // driftSource (Keel)
		fakeWorkspaces{ws: []WorkspaceModels{{WorkspaceID: "ws1", AllowedModels: []string{"A", "B", "C"}}}},
		store,
		func(string) float64 { return 1.0 }, // flat cost
	)
	if err := job.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	recs, err := store.LoadRecommendations(ctx)
	if err != nil {
		t.Fatalf("LoadRecommendations: %v", err)
	}
	byDiff := map[int]Recommendation{}
	for _, r := range recs {
		if r.Model == "C" {
			t.Errorf("Keel-adverse model C must be excluded; got %+v", r)
		}
		if !r.Verified {
			t.Errorf("computed rec must be Verified; got %+v", r)
		}
		byDiff[r.Difficulty] = r
	}
	if len(recs) != modelcapability.MaxDifficulty+1 {
		t.Fatalf("expected one rec per difficulty; got %d", len(recs))
	}
	if byDiff[0].Model != "A" {
		t.Errorf("d=0 pick should be A (0.90); got %q", byDiff[0].Model)
	}
	if byDiff[modelcapability.MaxDifficulty].Model != "B" {
		t.Errorf("d=%d pick should be B (A degraded below it); got %q", modelcapability.MaxDifficulty, byDiff[modelcapability.MaxDifficulty].Model)
	}
}
