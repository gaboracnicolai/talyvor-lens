package budgets

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

// B17.14 — a spending limit below what one request costs is kept as set, so a hard_block budget
// refuses the next request; switched off, the same request is let through. On the migrated schema,
// because the limit was lost in the column type: NUMERIC(12,4) stored $0.000001 as $0, and a zero
// limit is "no limit".
func TestSubCentLimit_RefusesTheNextRequest_OffLetsItThrough(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG sub-cent budget test")
	}
	const schema = "budgets_sub_cent_realpg"
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, ddl := range []string{`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`, `CREATE SCHEMA ` + schema} {
		if _, err := conn.Exec(ctx, ddl); err != nil {
			t.Fatalf("reset schema: %v", err)
		}
	}
	if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_ = conn.Close(ctx)
	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	store := NewStore(pool)
	svc := NewService(store)
	const ws = "ws-sub-cent"
	const oneQuestion = 0.0014 // what the testers' "What is 595 + 267?" cost

	b, err := store.Create(ctx, Budget{WorkspaceID: ws, Scope: ScopeWorkspace, LimitUSD: 0.000001, Enforcement: EnforcementHardBlock})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got, _ := store.Get(ctx, ws, b.ID); got == nil || got.LimitUSD != 0.000001 {
		t.Fatalf("the limit was stored as %+v, not $0.000001", got)
	}
	if d := svc.CheckBudget(ctx, ws, "", "", oneQuestion); d != DecisionBlock {
		t.Fatalf("past a $0.000001 limit the next request was %q, want %q", d, DecisionBlock)
	}

	// What the Features switch sends through the BFF: the budget as it is, enforcement off.
	if _, err := store.Update(ctx, ws, b.ID, Budget{Period: b.Period, LimitUSD: 0.000001, AlertThresholds: b.AlertThresholds, Enforcement: EnforcementOff}); err != nil {
		t.Fatalf("switch off: %v", err)
	}
	if err := svc.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if d := svc.CheckBudget(ctx, ws, "", "", oneQuestion); d != DecisionAllow {
		t.Fatalf("with the limit off the next request was %q, want %q", d, DecisionAllow)
	}
}
