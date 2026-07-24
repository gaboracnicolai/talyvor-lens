package workspace

import (
	"context"
	"os"
	"testing"
)

// RED-FIRST (in-memory, STORED value = the manager's record): a newly registered
// workspace defaults to cross-tenant poolable=true. Asserts the stored flag, not
// an API response. Before the default flip this was false.
func TestRegisterWorkspace_NewWorkspaceDefaultsPoolable(t *testing.T) {
	m := New(nil)
	if err := m.RegisterWorkspace(context.Background(), Workspace{ID: "fresh", Name: "F", Active: true}); err != nil {
		t.Fatal(err)
	}
	ws, ok := m.GetWorkspace("fresh")
	if !ok {
		t.Fatal("workspace not registered")
	}
	if !ws.CachePoolable {
		t.Error("a newly registered workspace must default to cache_poolable=true (stored value)")
	}
	if !m.GetCachePoolable("fresh") {
		t.Error("GetCachePoolable must report the new default (true) for a fresh workspace")
	}
}

// Re-registration must NEVER change an existing workspace's pooling consent — the
// new default applies only to genuinely NEW workspaces. An opted-out workspace
// (false) stays false across a blind re-POST, in-memory.
func TestRegisterWorkspace_ReRegistrationPreservesOptOut(t *testing.T) {
	m := New(nil)
	ctx := context.Background()
	_ = m.RegisterWorkspace(ctx, Workspace{ID: "w", Name: "W", Active: true}) // defaults true
	if err := m.SetCachePoolable(ctx, "w", false); err != nil {               // explicit opt-out
		t.Fatal(err)
	}
	_ = m.RegisterWorkspace(ctx, Workspace{ID: "w", Name: "W", Active: true}) // blind re-POST
	if m.GetCachePoolable("w") {
		t.Error("re-registration must not re-pool an opted-out workspace (existing consent preserved)")
	}
}

// RED-FIRST (real PG, STORED value = the persisted row): registration writes
// cache_poolable=true for a new workspace.
func TestRegisterWorkspace_NewWorkspacePoolable_RealPG(t *testing.T) {
	pool := restartTestPool(t)
	m := New(pool)
	if err := m.RegisterWorkspace(context.Background(), Workspace{ID: "fresh-pg", Name: "F"}); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}
	var stored bool
	if err := pool.QueryRow(context.Background(),
		`SELECT cache_poolable FROM workspaces WHERE id = 'fresh-pg'`).Scan(&stored); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !stored {
		t.Error("the persisted row for a new workspace must have cache_poolable=true")
	}
}

// Re-registration preserves an opted-out row at the DB level too (the ON CONFLICT
// clause must not overwrite cache_poolable): opt out, re-POST, still false.
func TestRegisterWorkspace_ReRegistrationPreservesOptOut_RealPG(t *testing.T) {
	pool := restartTestPool(t)
	ctx := context.Background()
	m := New(pool)
	if err := m.RegisterWorkspace(ctx, Workspace{ID: "ws-optout", Name: "O"}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetCachePoolable(ctx, "ws-optout", false); err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterWorkspace(ctx, Workspace{ID: "ws-optout", Name: "O", CachePoolable: false}); err != nil {
		t.Fatal(err)
	}
	var stored bool
	if err := pool.QueryRow(ctx,
		`SELECT cache_poolable FROM workspaces WHERE id = 'ws-optout'`).Scan(&stored); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored {
		t.Error("re-registration must not flip an opted-out row back to poolable (DB consent preserved)")
	}
}

// A blind re-POST that lands on a replica which NEVER loaded the row (its cache
// is cold) must not re-default an opted-out workspace back to poolable. The app
// can't tell "new" from "unknown-here" for a cold replica, so the DB ON CONFLICT
// clause is the guarantor: it preserves the stored cache_poolable. Asserts the
// persisted row. (RED with the app default but the old EXCLUDED overwrite.)
func TestRegisterWorkspace_ReRegisterFromColdReplica_PreservesOptOut_RealPG(t *testing.T) {
	pool := restartTestPool(t)
	ctx := context.Background()

	// Replica A creates the workspace and opts it OUT of pooling.
	mA := New(pool)
	if err := mA.RegisterWorkspace(ctx, Workspace{ID: "ws-cold", Name: "C"}); err != nil {
		t.Fatal(err)
	}
	if err := mA.SetCachePoolable(ctx, "ws-cold", false); err != nil {
		t.Fatal(err)
	}

	// Replica B is COLD — it never ran LoadAll, so its cache doesn't hold ws-cold.
	// A re-POST hitting B must NOT flip the persisted row back to poolable.
	mB := New(pool)
	if err := mB.RegisterWorkspace(ctx, Workspace{ID: "ws-cold", Name: "C"}); err != nil {
		t.Fatal(err)
	}
	var stored bool
	if err := pool.QueryRow(ctx,
		`SELECT cache_poolable FROM workspaces WHERE id = 'ws-cold'`).Scan(&stored); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored {
		t.Error("a re-POST on a replica that never loaded the row must not re-pool it (DB preserves consent)")
	}
	// And the boundary holds in memory too: RETURNING reconciles B's cache to the persisted
	// value, so B does not even transiently serve ws-cold as poolable.
	if mB.GetCachePoolable("ws-cold") {
		t.Error("B's in-memory flag must be reconciled to the persisted false, not the new default")
	}
}

// The 0106 migration flips ONLY the column DEFAULT for NEW rows: an EXISTING
// cache_poolable=false row stays false across it (no retroactive pooling), and a
// row inserted afterward that omits the column picks up the new default true.
// Applies the ACTUAL migration file so the SQL under test is the shipped one.
func TestMigration0106_DefaultTrue_ExistingRowUntouched(t *testing.T) {
	pool := restartTestPool(t) // workspaces table at the pre-0106 default (false)
	ctx := context.Background()

	// An EXISTING workspace that never consented to pooling.
	if _, err := pool.Exec(ctx,
		`INSERT INTO workspaces (id, name, cache_prefix, cache_poolable) VALUES ('mig-existing','E','p:',false)`); err != nil {
		t.Fatalf("seed existing row: %v", err)
	}

	// Apply the real 0106 migration (SET DEFAULT true — a catalog change, no UPDATE).
	sql, err := os.ReadFile("../../migrations/0106_workspace_cache_poolable_default_true.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("apply 0106: %v", err)
	}

	// Existing row is UNTOUCHED — SET DEFAULT never rewrites rows.
	var existing bool
	if err := pool.QueryRow(ctx,
		`SELECT cache_poolable FROM workspaces WHERE id = 'mig-existing'`).Scan(&existing); err != nil {
		t.Fatalf("read existing: %v", err)
	}
	if existing {
		t.Error("an existing cache_poolable=false row must STAY false across 0106 (consent is not retroactive)")
	}

	// A NEW row that omits the column picks up the flipped default (true).
	if _, err := pool.Exec(ctx,
		`INSERT INTO workspaces (id, name, cache_prefix) VALUES ('mig-postmig','P','p:')`); err != nil {
		t.Fatalf("insert post-migration row: %v", err)
	}
	var fresh bool
	if err := pool.QueryRow(ctx,
		`SELECT cache_poolable FROM workspaces WHERE id = 'mig-postmig'`).Scan(&fresh); err != nil {
		t.Fatalf("read post-migration: %v", err)
	}
	if !fresh {
		t.Error("a row inserted after 0106 that omits cache_poolable must default to true")
	}
}
