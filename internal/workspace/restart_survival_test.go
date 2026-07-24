package workspace

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// restartTestPool builds a real-PG pool and a fresh `workspaces` table mirroring
// the production schema (migrations 0005/0039/0041/0104). Skips when
// LENS_TEST_DATABASE_URL is unset.
func restartTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG workspace restart test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS workspaces`,
		`CREATE TABLE workspaces (
			id TEXT PRIMARY KEY, name TEXT NOT NULL, cache_prefix TEXT NOT NULL,
			spend_limit_usd FLOAT NOT NULL DEFAULT 0,
			allowed_models TEXT[] NOT NULL DEFAULT '{}',
			allowed_providers TEXT[] NOT NULL DEFAULT '{}',
			max_tokens_per_request INTEGER NOT NULL DEFAULT 0,
			max_output_tokens INTEGER NOT NULL DEFAULT 0,
			max_input_tokens INTEGER NOT NULL DEFAULT 0,
			active BOOLEAN NOT NULL DEFAULT true,
			logging_policy TEXT NOT NULL DEFAULT 'metadata',
			distill_policy TEXT NOT NULL DEFAULT 'disabled',
			cache_poolable BOOLEAN NOT NULL DEFAULT false,
			distill_poolable BOOLEAN NOT NULL DEFAULT false,
			cost_optimize_routing BOOLEAN NOT NULL DEFAULT false,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool
}

// TestWorkspacePolicy_SurvivesRestart_Integration is the #129 guard. A workspace
// registered with the omitted-body zero value (Active=false) and a LoggingNone
// privacy policy must STILL be loaded — and its policy honored — after a restart
// (a fresh Manager over the same DB, the boot LoadAll). On main this FAILS:
// LoadAll's WHERE active=true drops the row, and the serve path falls back to
// the metadata default — silently logging a customer who opted out.
func TestWorkspacePolicy_SurvivesRestart_Integration(t *testing.T) {
	pool := restartTestPool(t)
	ctx := context.Background()

	// m1: the gateway process where the customer registers + sets LoggingNone.
	// Active:false simulates POST /v1/workspaces with the field omitted.
	m1 := New(pool)
	if err := m1.RegisterWorkspace(ctx, Workspace{
		ID: "ws-none-restart", Name: "privacy customer",
		Active: false, LoggingPolicy: LoggingNone,
		AllowedModels: []string{}, AllowedProviders: []string{},
	}); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}
	if got := m1.GetLoggingPolicy("ws-none-restart"); got != LoggingNone {
		t.Fatalf("pre-restart policy = %q, want none", got)
	}

	// m2: a RESTARTED gateway — fresh Manager, boot reload from the same DB.
	m2 := New(pool)
	if err := m2.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if _, ok := m2.GetWorkspace("ws-none-restart"); !ok {
		t.Fatal("RESTART DROPPED the workspace: GetWorkspace not found after LoadAll")
	}
	if got := m2.GetLoggingPolicy("ws-none-restart"); got != LoggingNone {
		t.Fatalf("PRIVACY REGRESSION: post-restart policy = %q, want none — a LoggingNone customer is logged again", got)
	}
}

// TestRegisterWorkspace_MinimalBodyPersists_Integration is the #128 guard: a
// minimal {id,name} registration (nil slices) must persist, not 400 on the
// allowed_models NOT NULL constraint. RED on main (RegisterWorkspace passes nil
// -> NULL -> 23502).
func TestRegisterWorkspace_MinimalBodyPersists_Integration(t *testing.T) {
	pool := restartTestPool(t)
	ctx := context.Background()
	m := New(pool)
	// Minimal body: only ID + Name, slices nil (the Go zero value).
	if err := m.RegisterWorkspace(ctx, Workspace{ID: "ws-minimal", Name: "minimal"}); err != nil {
		t.Fatalf("minimal {id,name} registration must succeed (#128), got: %v", err)
	}
	var active bool
	var models []string
	if err := pool.QueryRow(ctx,
		`SELECT active, allowed_models FROM workspaces WHERE id='ws-minimal'`,
	).Scan(&active, &models); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !active {
		t.Error("#129: minimal registration must persist active=true")
	}
	if models == nil {
		t.Error("#128: allowed_models must persist as a non-null empty array")
	}
}

// TestRegisterWorkspace_ForcesActiveTrue pins the #129 fix: lifecycle is
// server-controlled — an omitted/false Active is forced to true. In-memory.
func TestRegisterWorkspace_ForcesActiveTrue(t *testing.T) {
	m := New(nil)
	if err := m.RegisterWorkspace(context.Background(), Workspace{
		ID: "w", Name: "W", Active: false,
	}); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}
	ws, ok := m.GetWorkspace("w")
	if !ok {
		t.Fatal("workspace not registered")
	}
	if !ws.Active {
		t.Error("#129: RegisterWorkspace must force Active=true (lifecycle is not caller-controlled)")
	}
}

// TestRegisterWorkspace_DefaultsNilSlices pins the #128 fix: nil
// AllowedModels/AllowedProviders become empty (non-nil) slices. In-memory.
func TestRegisterWorkspace_DefaultsNilSlices(t *testing.T) {
	m := New(nil)
	if err := m.RegisterWorkspace(context.Background(), Workspace{ID: "w", Name: "W"}); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}
	ws, _ := m.GetWorkspace("w")
	if ws.AllowedModels == nil || ws.AllowedProviders == nil {
		t.Error("#128: nil allowed_models/allowed_providers must default to empty (non-nil) slices")
	}
}

// TestRegisterWorkspace_ReRegistrationUpdatesPolicy pins the chosen semantics of
// the ON CONFLICT upsert: re-registering an existing workspace UPDATES its
// logging_policy to the new body's value. This is intended (re-registration is a
// full-record upsert) — pinned so a future reader doesn't mistake it for a bug.
// The active half is closed by construction (forced true), so a re-POST can
// never silently deactivate.
func TestRegisterWorkspace_ReRegistrationUpdatesPolicy(t *testing.T) {
	m := New(nil)
	ctx := context.Background()
	_ = m.RegisterWorkspace(ctx, Workspace{ID: "w", Name: "W", LoggingPolicy: LoggingNone})
	if got := m.GetLoggingPolicy("w"); got != LoggingNone {
		t.Fatalf("setup: policy = %q, want none", got)
	}
	// Re-register with a different policy — the upsert updates it.
	_ = m.RegisterWorkspace(ctx, Workspace{ID: "w", Name: "W", LoggingPolicy: LoggingFull})
	if got := m.GetLoggingPolicy("w"); got != LoggingFull {
		t.Errorf("re-registration should update policy to full, got %q", got)
	}
}
