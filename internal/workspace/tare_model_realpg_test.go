package workspace

import (
	"context"
	"os"
	"testing"
)

// B27.35 — Tare phase 2a's model is OFF unless the workspace opts in, existing workspaces included,
// and once a workspace opts in, re-registration does not undo it.
func TestTareModel_OffUntilOptedIn_AndOptInSurvives_Integration(t *testing.T) {
	pool := restartTestPool(t)
	ctx := context.Background()

	// The shipped migration, on the pre-0184 shape: an existing workspace and a new one both read false.
	if _, err := pool.Exec(ctx, `ALTER TABLE workspaces DROP COLUMN tare_model`); err != nil {
		t.Fatalf("restore the pre-0184 shape: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ('ws-existing','E','e:')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sql, err := os.ReadFile("../../migrations/0184_workspace_tare_model.sql")
	if err != nil {
		t.Fatalf("read the migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("apply 0184: %v", err)
	}

	m1 := New(pool)
	if err := m1.RegisterWorkspace(ctx, Workspace{ID: "ws-new", Name: "new"}); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}
	if err := m1.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	for _, id := range []string{"ws-existing", "ws-new"} {
		if m1.GetTareModel(id) {
			t.Errorf("%s: tare model is on without opting in", id)
		}
	}

	if err := m1.SetTareModel(ctx, "ws-new", true); err != nil {
		t.Fatalf("SetTareModel: %v", err)
	}
	m2 := New(pool) // a replica that never cached the row, re-registering it the way boot does
	if err := m2.RegisterWorkspace(ctx, Workspace{ID: "ws-new", Name: "new"}); err != nil {
		t.Fatalf("re-RegisterWorkspace: %v", err)
	}
	if !m2.GetTareModel("ws-new") {
		t.Errorf("re-registration switched the tare model off in memory")
	}
	m3 := New(pool)
	if err := m3.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if !m3.GetTareModel("ws-new") || m3.GetTareModel("ws-existing") {
		t.Errorf("after restart: ws-new=%v ws-existing=%v, want true and false", m3.GetTareModel("ws-new"), m3.GetTareModel("ws-existing"))
	}
}
