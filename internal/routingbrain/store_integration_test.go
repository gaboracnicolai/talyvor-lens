package routingbrain

import (
	"context"
	"os"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func rbTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG routingbrain test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS routing_brain_recommendations`,
		`DROP TABLE IF EXISTS routing_brain_autonomous`,
		`CREATE TABLE routing_brain_recommendations (
			workspace_id TEXT NOT NULL, difficulty INTEGER NOT NULL,
			model TEXT NOT NULL, provider TEXT NOT NULL DEFAULT '',
			expected_quality DOUBLE PRECISION NOT NULL DEFAULT 0, verified BOOLEAN NOT NULL DEFAULT FALSE,
			reason TEXT NOT NULL DEFAULT '', computed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (workspace_id, difficulty))`,
		`CREATE TABLE routing_brain_autonomous (
			workspace_id TEXT PRIMARY KEY, opted_in_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool
}

// TestStore_Recommendations_UpsertLoadRoundTrip — Upsert persists a recommendation
// and LoadRecommendations reads it back; a second Upsert of the SAME (workspace,
// difficulty) UPDATES in place (no duplicate rows).
func TestStore_Recommendations_UpsertLoadRoundTrip(t *testing.T) {
	pool := rbTestPool(t)
	ctx := context.Background()
	s := NewStore(pool)

	if err := s.Upsert(ctx, Recommendation{WorkspaceID: "ws1", Difficulty: 3, Model: "A", Provider: "openai", ExpectedQuality: 0.8, Verified: true, Reason: "r"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// Re-upsert same key with a different model — must update, not duplicate.
	if err := s.Upsert(ctx, Recommendation{WorkspaceID: "ws1", Difficulty: 3, Model: "B", Provider: "openai", ExpectedQuality: 0.9, Verified: true, Reason: "r2"}); err != nil {
		t.Fatalf("Upsert 2: %v", err)
	}
	if err := s.Upsert(ctx, Recommendation{WorkspaceID: "ws1", Difficulty: 4, Model: "C", Provider: "anthropic", ExpectedQuality: 0.7, Verified: false, Reason: "r3"}); err != nil {
		t.Fatalf("Upsert 3: %v", err)
	}

	got, err := s.LoadRecommendations(ctx)
	if err != nil {
		t.Fatalf("LoadRecommendations: %v", err)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].Difficulty < got[j].Difficulty })
	if len(got) != 2 {
		t.Fatalf("expected 2 rows (ws1/d3 updated, ws1/d4); got %d: %+v", len(got), got)
	}
	if got[0].Difficulty != 3 || got[0].Model != "B" || got[0].ExpectedQuality != 0.9 || !got[0].Verified {
		t.Errorf("d3 must reflect the UPDATE to model B; got %+v", got[0])
	}
	if got[1].Difficulty != 4 || got[1].Model != "C" || got[1].Verified {
		t.Errorf("d4 row wrong; got %+v", got[1])
	}
}

// TestStore_Autonomous_OptInReadClear — the per-workspace autonomous switch:
// SetAutonomous opts a workspace in, LoadAutonomous reads the opted-in set, and
// ClearAutonomous removes it. Absence = advisory (the default).
func TestStore_Autonomous_OptInReadClear(t *testing.T) {
	pool := rbTestPool(t)
	ctx := context.Background()
	s := NewStore(pool)

	if err := s.SetAutonomous(ctx, "wsAuto"); err != nil {
		t.Fatalf("SetAutonomous: %v", err)
	}
	if err := s.SetAutonomous(ctx, "wsAuto"); err != nil { // idempotent
		t.Fatalf("SetAutonomous idempotent: %v", err)
	}
	set, err := s.LoadAutonomous(ctx)
	if err != nil {
		t.Fatalf("LoadAutonomous: %v", err)
	}
	if len(set) != 1 || set[0] != "wsAuto" {
		t.Fatalf("expected {wsAuto}; got %v", set)
	}
	if err := s.ClearAutonomous(ctx, "wsAuto"); err != nil {
		t.Fatalf("ClearAutonomous: %v", err)
	}
	set, err = s.LoadAutonomous(ctx)
	if err != nil {
		t.Fatalf("LoadAutonomous after clear: %v", err)
	}
	if len(set) != 0 {
		t.Errorf("cleared workspace must not be autonomous; got %v", set)
	}
}

// TestStore_NilPool_NoOp — a nil-pool store is inert.
func TestStore_NilPool_NoOp(t *testing.T) {
	s := NewStore(nil)
	ctx := context.Background()
	if err := s.Upsert(ctx, Recommendation{WorkspaceID: "w", Difficulty: 0, Model: "m"}); err != nil {
		t.Errorf("nil Upsert must no-op, got %v", err)
	}
	if err := s.SetAutonomous(ctx, "w"); err != nil {
		t.Errorf("nil SetAutonomous must no-op, got %v", err)
	}
	if recs, err := s.LoadRecommendations(ctx); err != nil || recs != nil {
		t.Errorf("nil LoadRecommendations must be empty no-op, got %v err=%v", recs, err)
	}
	if set, err := s.LoadAutonomous(ctx); err != nil || set != nil {
		t.Errorf("nil LoadAutonomous must be empty no-op, got %v err=%v", set, err)
	}
}
