package keel

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SEC-4/SEC-5 (real PG): ListFindingsForWorkspace returns ONLY the named workspace's rows — seed A and B, read
// A, prove B never appears. Also: A's ordinary AND hardened rows both return, correctly labeled.
func scopePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG keel workspace-scope test")
	}
	// PRIVATE schema: the breach suite (keelTestPool) migrates the REAL keel_findings (metrics JSONB NOT NULL,
	// …) into the package schema. This test needs a minimal keel_findings and must NOT clobber that shared
	// table, so it isolates via its own search_path (overriding the package TestMain's).
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "lens_it_keelscope"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), `
		DROP SCHEMA IF EXISTS lens_it_keelscope CASCADE; CREATE SCHEMA lens_it_keelscope;
		CREATE TABLE keel_findings (
			id BIGSERIAL PRIMARY KEY, workspace_id TEXT NOT NULL, unit TEXT NOT NULL, window_bucket BIGINT NOT NULL,
			deviation_sigma DOUBLE PRECISION NOT NULL, attribution TEXT NOT NULL, cohort_n INTEGER NOT NULL DEFAULT 0,
			identity_key TEXT NOT NULL UNIQUE, mode TEXT NOT NULL DEFAULT 'ordinary',
			first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return pool
}

func seedFinding(t *testing.T, pool *pgxpool.Pool, ws, attribution, mode, key string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO keel_findings (workspace_id, unit, window_bucket, deviation_sigma, attribution, cohort_n, identity_key, mode)
		 VALUES ($1,'model_used',100,-3.0,$2,12,$3,$4)`, ws, attribution, key, mode); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestListFindingsForWorkspace_TenantIsolated(t *testing.T) {
	pool := scopePool(t)
	ctx := context.Background()
	seedFinding(t, pool, "ws-A", "idiosyncratic", "hardened", "kA1")
	seedFinding(t, pool, "ws-A", "common_mode", "ordinary", "kA2")
	seedFinding(t, pool, "ws-B", "idiosyncratic", "hardened", "kB1")

	got, err := NewReader(pool).ListFindingsForWorkspace(ctx, "ws-A", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("A got %d rows, want 2 (only A's — B must not appear)", len(got))
	}
	var sawHardened, sawOrdinary bool
	for _, f := range got {
		if f.WorkspaceID != "ws-A" {
			t.Errorf("SEC-4/5 BREACH: A's read returned workspace %q", f.WorkspaceID)
		}
		switch f.Mode {
		case "hardened":
			sawHardened = true
			if f.Attribution != "idiosyncratic" {
				t.Errorf("hardened row attribution=%q, want idiosyncratic", f.Attribution)
			}
		case "ordinary":
			sawOrdinary = true
			if f.Attribution != "common_mode" {
				t.Errorf("ordinary row attribution=%q, want common_mode", f.Attribution)
			}
		}
	}
	if !sawHardened || !sawOrdinary {
		t.Errorf("both modes must return, labeled (hardened=%v ordinary=%v)", sawHardened, sawOrdinary)
	}

	// B sees only B's one row.
	gotB, _ := NewReader(pool).ListFindingsForWorkspace(ctx, "ws-B", 100)
	if len(gotB) != 1 || gotB[0].WorkspaceID != "ws-B" {
		t.Errorf("B got %d rows (want 1, only B's)", len(gotB))
	}
}
