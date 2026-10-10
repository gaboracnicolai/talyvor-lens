package workspace

import (
	"context"
	"os"
	"testing"
)

// B27.36 — Tare phase 2b's training set holds prose from workspaces that opted in and from synthetic test
// workspaces, and nothing from a workspace that did not opt in. Switching the opt-in off removes the workspace's
// traces at once and stops collection, even on a replica whose cache still says on.
func TestTareTraining_NotOptedInContributesNothing_OffForgets_Integration(t *testing.T) {
	pool := restartTestPool(t)
	ctx := context.Background()
	for _, ddl := range []string{`DROP TABLE IF EXISTS tare_training_switches`, `DROP TABLE IF EXISTS tare_training_traces`} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	sql, err := os.ReadFile("../../migrations/0243_tare_training.sql")
	if err != nil {
		t.Fatalf("read the migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("apply 0243: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES
		('ws-off','O','o:',false), ('ws-on','I','i:',false), ('ws-syn','S','s:',true)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m := New(pool)
	if err := m.LoadAll(ctx); err != nil {
		t.Fatal(err)
	}
	if m.GetTareTraining("ws-off") || m.GetTareTraining("ws-on") || m.CollectsTareTraining("ws-off") {
		t.Fatalf("training is on for a workspace that never opted in")
	}
	if _, err := m.SetTareTraining(ctx, "ws-on", true, "key:bff", "user:owner-1"); err != nil {
		t.Fatal(err)
	}
	var by, forWhom string
	if err := pool.QueryRow(ctx, `SELECT changed_by, on_behalf_of FROM tare_training_switches WHERE workspace_id='ws-on' AND enabled`).Scan(&by, &forWhom); err != nil || by != "key:bff" || forWhom != "user:owner-1" {
		t.Errorf("who switched it on = %q for %q (%v), want key:bff for user:owner-1", by, forWhom, err)
	}

	for _, ws := range []string{"ws-off", "ws-on", "ws-syn", "ws-on"} {
		if err := m.AddTareTrace(ctx, ws, "prose from "+ws); err != nil {
			t.Fatal(err)
		}
	}
	set, err := m.TareTrainingSet(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []TareTrace{{"ws-on", "prose from ws-on"}, {"ws-syn", "prose from ws-syn"}}
	if len(set) != len(want) || set[0] != want[0] || set[1] != want[1] {
		t.Errorf("training set = %+v, want %+v", set, want)
	}
	var offRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tare_training_traces WHERE workspace_id='ws-off'`).Scan(&offRows); err != nil || offRows != 0 {
		t.Errorf("a workspace that never opted in has %d stored traces (%v), want 0", offRows, err)
	}

	stale := New(pool) // a replica that loaded the opt-in while it was on
	if err := stale.LoadAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetTareTraining(ctx, "ws-on", false, "key:bff", "user:owner-1"); err != nil {
		t.Fatal(err)
	}
	if !stale.CollectsTareTraining("ws-on") {
		t.Fatal("control: the stale replica should still believe ws-on collects")
	}
	if err := stale.AddTareTrace(ctx, "ws-on", "prose after the switch-off"); err != nil {
		t.Fatal(err)
	}
	var onRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tare_training_traces WHERE workspace_id='ws-on'`).Scan(&onRows); err != nil || onRows != 0 {
		t.Errorf("after switching off, ws-on has %d stored traces (%v), want 0", onRows, err)
	}
	if set, _ := m.TareTrainingSet(ctx); len(set) != 1 || set[0].WorkspaceID != "ws-syn" {
		t.Errorf("training set after the switch-off = %+v, want only ws-syn", set)
	}
}
