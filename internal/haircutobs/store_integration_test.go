package haircutobs

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Real-PG: an applied haircut (a ledger row carrying drift_haircut_factor) is surfaced by Recent, joined to
// the workspace's hardened finding. A ledger row WITHOUT the factor is NOT surfaced (only real haircuts show).
func obsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG haircutobs test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), `
		DROP TABLE IF EXISTS lens_token_ledger;
		DROP TABLE IF EXISTS keel_findings;
		CREATE TABLE lens_token_ledger (
			id UUID NOT NULL DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL, amount BIGINT NOT NULL,
			balance_after BIGINT NOT NULL DEFAULT 0, type TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
			metadata JSONB NOT NULL DEFAULT '{}'::jsonb, created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (id, workspace_id));
		CREATE TABLE keel_findings (
			id BIGSERIAL PRIMARY KEY, workspace_id TEXT NOT NULL, unit TEXT NOT NULL, window_bucket BIGINT NOT NULL,
			deviation_sigma DOUBLE PRECISION NOT NULL, attribution TEXT NOT NULL, identity_key TEXT NOT NULL UNIQUE,
			mode TEXT NOT NULL DEFAULT 'ordinary', first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return pool
}

func TestRecent_SurfacesAppliedHaircut_WithCause(t *testing.T) {
	pool := obsPool(t)
	ctx := context.Background()
	// a hardened finding (the cause) + a bonded mint that was haircut (factor 0.5)
	if _, err := pool.Exec(ctx, `INSERT INTO keel_findings
		(workspace_id, unit, window_bucket, deviation_sigma, attribution, identity_key, mode)
		VALUES ('wsH','model_used',100,-3.5,'idiosyncratic','k1','hardened')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO lens_token_ledger (workspace_id, amount, type, metadata)
		VALUES ('wsH', 5000000, 'pool_royalty_held',
			'{"drift_haircut_factor":0.5,"reputation_base_ulens":10000000,"reputation_effective_ulens":5000000}'::jsonb)`); err != nil {
		t.Fatal(err)
	}
	// a NON-haircut mint (no factor) must NOT appear
	if _, err := pool.Exec(ctx, `INSERT INTO lens_token_ledger (workspace_id, amount, type, metadata)
		VALUES ('wsClean', 9000000, 'pool_royalty_held', '{}'::jsonb)`); err != nil {
		t.Fatal(err)
	}

	events, err := NewReader(pool).Recent(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d haircut events, want 1 (only the row with drift_haircut_factor)", len(events))
	}
	e := events[0]
	if e.WorkspaceID != "wsH" || e.Factor != 0.5 {
		t.Errorf("event = %+v, want wsH factor 0.5", e)
	}
	if e.BaseULens != 10000000 || e.EffectiveULens != 5000000 {
		t.Errorf("base/effective = %d/%d, want 10000000/5000000", e.BaseULens, e.EffectiveULens)
	}
	if e.DeviationSigma == nil || *e.DeviationSigma != -3.5 {
		t.Errorf("deviation_sigma = %v, want -3.5 (joined from the causing hardened finding)", e.DeviationSigma)
	}
}
