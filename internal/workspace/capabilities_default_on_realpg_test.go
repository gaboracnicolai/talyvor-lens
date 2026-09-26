package workspace

import (
	"context"
	"os"
	"testing"
)

// B8.3 — Tare and document conversion are ON by default for new AND existing workspaces, and a
// workspace that turns one off stays off.

// TestMigration0134_TareAndDistillOnForExistingAndNewWorkspaces applies the shipped migration to the
// pre-0134 shape: a workspace that never chose is moved to 'always', one that chose opt_in keeps it,
// and a row inserted afterwards gets 'always' from the column default.
func TestMigration0134_TareAndDistillOnForExistingAndNewWorkspaces(t *testing.T) {
	pool := restartTestPool(t)
	ctx := context.Background()

	// The pre-0134 shape: both columns defaulted to 'disabled' (0039, 0126).
	if _, err := pool.Exec(ctx, `ALTER TABLE workspaces
		ALTER COLUMN tare_policy SET DEFAULT 'disabled', ALTER COLUMN distill_policy SET DEFAULT 'disabled'`); err != nil {
		t.Fatalf("restore the pre-0134 defaults: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ('mig-never-chose','N','n:')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, tare_policy, distill_policy)
		VALUES ('mig-opt-in','O','o:','opt_in','opt_in')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	sql, err := os.ReadFile("../../migrations/0134_workspace_tare_distill_default_always.sql")
	if err != nil {
		t.Fatalf("read the migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("apply 0134: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ('mig-new','W','w:')`); err != nil {
		t.Fatalf("insert after 0134: %v", err)
	}

	for id, want := range map[string]string{"mig-never-chose": "always", "mig-opt-in": "opt_in", "mig-new": "always"} {
		var tare, distill string
		if err := pool.QueryRow(ctx, `SELECT tare_policy, distill_policy FROM workspaces WHERE id=$1`, id).Scan(&tare, &distill); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if tare != want || distill != want {
			t.Errorf("%s: tare_policy=%q distill_policy=%q, want both %q", id, tare, distill, want)
		}
	}
}

// TestRegisterWorkspace_ReRegistrationKeepsTareAndDistillOff_Integration: a brand-new workspace is
// on without touching a setting; after the customer turns both off, a blind re-registration on a
// replica that never cached the row (the boot default-workspace registration does this on every
// start) leaves them off — in that replica's memory, in the DB, and after a restart.
func TestRegisterWorkspace_ReRegistrationKeepsTareAndDistillOff_Integration(t *testing.T) {
	pool := restartTestPool(t)
	ctx := context.Background()

	m1 := New(pool)
	if err := m1.RegisterWorkspace(ctx, Workspace{ID: "ws-b83", Name: "b83"}); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}
	if got := m1.GetTarePolicy("ws-b83"); got != TareAlways {
		t.Fatalf("new workspace Tare = %q, want always", got)
	}
	if got := m1.GetDistillPolicy("ws-b83"); got != DistillAlways {
		t.Fatalf("new workspace distill = %q, want always", got)
	}
	if err := m1.SetTarePolicy(ctx, "ws-b83", TareDisabled); err != nil {
		t.Fatalf("SetTarePolicy: %v", err)
	}
	if err := m1.SetDistillPolicy(ctx, "ws-b83", DistillDisabled); err != nil {
		t.Fatalf("SetDistillPolicy: %v", err)
	}

	m2 := New(pool)
	if err := m2.RegisterWorkspace(ctx, Workspace{ID: "ws-b83", Name: "b83"}); err != nil {
		t.Fatalf("re-RegisterWorkspace: %v", err)
	}
	if got := m2.GetTarePolicy("ws-b83"); got != TareDisabled {
		t.Errorf("re-registration switched Tare back on in memory: %q, want disabled", got)
	}
	if got := m2.GetDistillPolicy("ws-b83"); got != DistillDisabled {
		t.Errorf("re-registration switched distill back on in memory: %q, want disabled", got)
	}

	m3 := New(pool)
	if err := m3.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if tare, distill := m3.GetTarePolicy("ws-b83"), m3.GetDistillPolicy("ws-b83"); tare != TareDisabled || distill != DistillDisabled {
		t.Errorf("after restart Tare=%q distill=%q, want both disabled", tare, distill)
	}
}
