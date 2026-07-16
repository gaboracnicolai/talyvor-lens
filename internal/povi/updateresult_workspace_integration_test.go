package povi_test

// updateresult_workspace_integration_test.go — Property B (money-path identity hardening).
//
// UpdateResult writes a challenge's final outcome + slash amount to its claimed row. The write
// must be CONFINED to the owning workspace: naming workspace A must never settle (and slash)
// workspace B's challenge, even if a wrong/foreign challenge id reaches this primitive. The
// slashed_amount (µLENS, SEC-2) is passed through UNCHANGED — this hardens only WHICH row the
// write may touch, keying on (id, workspace_id) not id alone (the SEC-11 lesson). No amount math.

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/povi"
)

const updateResultSchema = "lens_updateresult_ws_test"

func updateResultPGStore(t *testing.T) (*povi.ChallengeStore, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG UpdateResult workspace-scope test")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = updateResultSchema
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ctx := context.Background()
	// Faithful subset of migration 0033's povi_challenges — slashed_amount keeps its prod type.
	for _, ddl := range []string{
		`DROP SCHEMA IF EXISTS ` + updateResultSchema + ` CASCADE`,
		`CREATE SCHEMA ` + updateResultSchema,
		`CREATE TABLE povi_challenges (
			id TEXT PRIMARY KEY,
			request_id TEXT NOT NULL UNIQUE,
			node_id TEXT NOT NULL,
			workspace_id TEXT NOT NULL,
			positions TEXT NOT NULL DEFAULT '',
			result TEXT NOT NULL,
			slashed_amount DOUBLE PRECISION NOT NULL DEFAULT 0,
			reason TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("ddl: %v", err)
		}
	}
	return povi.NewChallengeStore(pool), pool
}

func seedPendingChallenge(t *testing.T, pool *pgxpool.Pool, id, ws string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO povi_challenges (id, request_id, node_id, workspace_id, result) VALUES ($1, $2, 'node', $3, 'pending')`,
		id, "req-"+id, ws); err != nil {
		t.Fatalf("seed challenge %s/%s: %v", id, ws, err)
	}
}

func challengeState(t *testing.T, pool *pgxpool.Pool, id string) (result string, slashed float64) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT result, slashed_amount FROM povi_challenges WHERE id=$1`, id).Scan(&result, &slashed); err != nil {
		t.Fatalf("read challenge %s: %v", id, err)
	}
	return result, slashed
}

// TestUpdateResult_ConfinedToWorkspace proves a slash/settle write cannot cross workspaces.
func TestUpdateResult_ConfinedToWorkspace(t *testing.T) {
	st, pool := updateResultPGStore(t)
	ctx := context.Background()
	seedPendingChallenge(t, pool, "chalA", "wsA")
	seedPendingChallenge(t, pool, "chalB", "wsB")

	// CROSS-WORKSPACE: name wsA but target wsB's challenge → must be refused; wsB's row unchanged.
	if err := st.UpdateResult(ctx, "wsA", "chalB", povi.ChallengeFail, 500, "cross-ws attempt"); err != nil {
		t.Fatalf("UpdateResult (cross-workspace) returned error: %v", err)
	}
	if r, s := challengeState(t, pool, "chalB"); r != "pending" || s != 0 {
		t.Fatalf("CROSS-TENANT SLASH: naming wsA settled wsB's challenge (result=%q slashed=%v), want pending/0 — "+
			"the slash/settle write must be confined to the owning workspace (id alone is not enough)", r, s)
	}

	// SAME-WORKSPACE: the legitimate settle path still writes result + slash.
	if err := st.UpdateResult(ctx, "wsB", "chalB", povi.ChallengeFail, 500, "legit"); err != nil {
		t.Fatalf("UpdateResult (same-workspace): %v", err)
	}
	if r, s := challengeState(t, pool, "chalB"); r != "fail" || s != 500 {
		t.Fatalf("same-workspace UpdateResult wrote result=%q slashed=%v, want fail/500 — legitimate settle must succeed", r, s)
	}
	// wsA's own challenge was never touched.
	if r, s := challengeState(t, pool, "chalA"); r != "pending" || s != 0 {
		t.Fatalf("wsA's challenge changed to result=%q slashed=%v, want pending/0 (never targeted)", r, s)
	}
}
