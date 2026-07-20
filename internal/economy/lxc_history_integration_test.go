package economy

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

var lxcHistMigrateOnce sync.Once

// lxcHistPool migrates the REAL schema into a private schema (lxc_ledger is built across 0027 + 0034 +
// 0083 → BIGINT µLXC) and returns a pool pinned to it.
func lxcHistPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG lxc history test")
	}
	const schema = "lxc_history_realpg"
	ctx := context.Background()
	lxcHistMigrateOnce.Do(func() {
		cfg, err := pgx.ParseConfig(url)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		cfg.RuntimeParams["search_path"] = schema + ",public"
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		defer conn.Close(ctx)
		// Fresh schema each process (lxc_ledger is append-only — no DELETE/UPDATE for cleanup — so a
		// persisted schema would accumulate rows across runs). DROP is DDL, unaffected by the trigger.
		for _, ddl := range []string{`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`, `CREATE SCHEMA ` + schema} {
			if _, err := conn.Exec(ctx, ddl); err != nil {
				t.Fatalf("reset schema: %v", err)
			}
		}
		if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	})
	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("pool cfg: %v", err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// GetLXCHistory mirrors tokens/history for the fiat LXC ledger: newest-first, limit+offset, workspace-scoped,
// integer µLXC. It is the first reader of lxc_ledger (all prior access was INSERT-only).
func TestGetLXCHistory_ScopesPaginatesAndIsInteger(t *testing.T) {
	ctx := context.Background()
	pool := lxcHistPool(t)
	dt := NewDualTokenStore(nil, pool, nil)

	// Seed lxc_ledger directly with distinct created_at (the table is append-only — no UPDATE — and
	// partitioned by workspace_id; a plain INSERT routes and is allowed). wsA: three rows with distinct
	// µLXC amounts and increasing created_at; wsB: one row (the cross-tenant control).
	seed := func(ws string, amount, balanceAfter int64, secs int) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO lxc_ledger (workspace_id, amount, balance_after, type, description, created_at)
			 VALUES ($1,$2,$3,'purchase','seed', TIMESTAMPTZ '2026-01-01 00:00:00+00' + make_interval(secs => $4::float8))`,
			ws, amount, balanceAfter, secs); err != nil {
			t.Fatalf("seed %s/%d: %v", ws, amount, err)
		}
	}
	seed("wsA-hist", 1_000_000, 1_000_000, 1)
	seed("wsA-hist", 2_000_000, 3_000_000, 2)
	seed("wsA-hist", 3_000_000, 6_000_000, 3)
	seed("wsB-hist", 9_000_000, 9_000_000, 1)

	// Scope: wsA sees its 3 rows, integer µLXC, newest-first; wsB never appears.
	all, err := dt.GetLXCHistory(ctx, "wsA-hist", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("wsA history: got %d rows, want 3", len(all))
	}
	if all[0].Amount != 3_000_000 || all[2].Amount != 1_000_000 {
		t.Errorf("must be newest-first by created_at: got amounts %d..%d", all[0].Amount, all[2].Amount)
	}
	for _, e := range all {
		if e.WorkspaceID != "wsA-hist" {
			t.Fatalf("cross-tenant leak: row for %q in wsA history", e.WorkspaceID)
		}
	}

	// Cross-tenant: wsB's history is ONLY wsB's row (workspace-scoped SELECT).
	b, err := dt.GetLXCHistory(ctx, "wsB-hist", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 1 || b[0].Amount != 9_000_000 {
		t.Fatalf("wsB history must be exactly its own 1 row; got %+v", b)
	}

	// Pagination: page 1 (limit 2) then page 2 (offset 2); disjoint, page 2 has the remaining 1.
	p1, _ := dt.GetLXCHistory(ctx, "wsA-hist", 2, 0)
	p2, _ := dt.GetLXCHistory(ctx, "wsA-hist", 2, 2)
	if len(p1) != 2 || len(p2) != 1 {
		t.Fatalf("pagination: page1=%d page2=%d, want 2 and 1", len(p1), len(p2))
	}
	if p1[0].Amount != 3_000_000 || p1[1].Amount != 2_000_000 || p2[0].Amount != 1_000_000 {
		t.Errorf("pages overlap or misordered: p1=%d,%d p2=%d", p1[0].Amount, p1[1].Amount, p2[0].Amount)
	}
}
