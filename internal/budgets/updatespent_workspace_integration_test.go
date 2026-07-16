package budgets

// updatespent_workspace_integration_test.go — Property B (money-path identity hardening).
//
// UpdateSpent persists a budget's reconciled spend (spent_usd — a Tier-3 ADVISORY USD snapshot,
// NOT conserved µLENS; SEC-2 money types are untouched here). The write must be CONFINED to the
// owning workspace: a caller naming workspace A must never be able to move workspace B's budget,
// even if a wrong/foreign budget id reaches this primitive. The SEC-11 lesson — a scoping key
// must contain every identity it protects — so the WHERE keys on (id, workspace_id), not id alone.

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const updateSpentSchema = "lens_updatespent_ws_test"

func updateSpentPGStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG UpdateSpent workspace-scope test")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = updateSpentSchema
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ctx := context.Background()
	// Faithful subset of migration 0028's budgets table — spent_usd keeps its prod type NUMERIC(12,4).
	for _, ddl := range []string{
		`DROP SCHEMA IF EXISTS ` + updateSpentSchema + ` CASCADE`,
		`CREATE SCHEMA ` + updateSpentSchema,
		`CREATE TABLE budgets (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			workspace_id TEXT NOT NULL,
			scope TEXT NOT NULL DEFAULT 'workspace',
			scope_id TEXT NOT NULL DEFAULT '',
			spent_usd NUMERIC(12,4) NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("ddl: %v", err)
		}
	}
	return NewStore(pool), pool
}

func seedBudget(t *testing.T, pool *pgxpool.Pool, ws string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO budgets (workspace_id, spent_usd) VALUES ($1, 0) RETURNING id::text`, ws).Scan(&id); err != nil {
		t.Fatalf("seed budget for %s: %v", ws, err)
	}
	return id
}

func spentOf(t *testing.T, pool *pgxpool.Pool, id string) float64 {
	t.Helper()
	var v float64
	if err := pool.QueryRow(context.Background(),
		`SELECT spent_usd FROM budgets WHERE id=$1::uuid`, id).Scan(&v); err != nil {
		t.Fatalf("read spent_usd for %s: %v", id, err)
	}
	return v
}

// TestUpdateSpent_ConfinedToWorkspace proves the money-path write cannot cross workspaces.
func TestUpdateSpent_ConfinedToWorkspace(t *testing.T) {
	st, pool := updateSpentPGStore(t)
	ctx := context.Background()
	budA := seedBudget(t, pool, "wsA")
	budB := seedBudget(t, pool, "wsB")

	// CROSS-WORKSPACE: name wsA but target wsB's budget id → must be refused; wsB's spend unchanged.
	if err := st.UpdateSpent(ctx, "wsA", budB, 999); err != nil {
		t.Fatalf("UpdateSpent (cross-workspace) returned error: %v", err)
	}
	if got := spentOf(t, pool, budB); got != 0 {
		t.Fatalf("CROSS-TENANT WRITE: naming wsA moved wsB's budget spent_usd to %v, want 0 (unchanged) — "+
			"the money-path write must be confined to the owning workspace (id alone is not enough)", got)
	}

	// SAME-WORKSPACE: the legitimate reconciliation path still writes.
	if err := st.UpdateSpent(ctx, "wsB", budB, 999); err != nil {
		t.Fatalf("UpdateSpent (same-workspace): %v", err)
	}
	if got := spentOf(t, pool, budB); got != 999 {
		t.Fatalf("same-workspace UpdateSpent wrote %v, want 999 — the legitimate write must succeed", got)
	}
	// wsA's own budget was never touched by any of the above.
	if got := spentOf(t, pool, budA); got != 0 {
		t.Fatalf("wsA's budget spent_usd = %v, want 0 (never targeted)", got)
	}
}
