package workspace

import (
	"context"
	"os"
	"testing"
)

// WHY THIS FILE EXISTS SEPARATELY FROM compression_policy_test.go.
//
// Those tests run the policy through an in-memory Manager (`New(nil)`), where the
// map IS the store. Nothing in them can see the two places the column actually
// has to work: the INSERT that persists an opt-in and the boot-time LoadAll that
// reads it back. A policy that is written and never re-read is not a policy — it
// is a value that lasts until the next deploy, and it would look identical to a
// working one in every in-memory test in this package.

// 0117 IS A BACKFILL DECISION, NOT JUST A COLUMN. Every workspace that exists
// today had its prompts rewritten without ever being asked; the migration must
// land all of them on 'disabled' rather than carrying the old behaviour forward
// under a new name. Applies the SHIPPED migration file to a pre-0117 table, so
// the SQL under test is the SQL that will run in production.
func TestMigration0117_ExistingWorkspacesBackfillDisabled(t *testing.T) {
	pool := restartTestPool(t) // production shape — which now INCLUDES the column
	ctx := context.Background()

	// Reconstruct the pre-0117 shape by removing it again.
	if _, err := pool.Exec(ctx, `ALTER TABLE workspaces DROP COLUMN compression_policy`); err != nil {
		t.Fatalf("drop to the pre-0117 shape: %v", err)
	}
	// PREMISE, ASSERTED: the column is really gone. Without this the test could
	// pass by reading a value that was never migrated into existence.
	//
	// ⚠ current_schema() IS LOAD-BEARING. This package runs under
	// search_path=lens_it_workspace (schema_isolation_test.go), and every other
	// integration package builds its own `workspaces` in its own schema — sixteen
	// of them carried this column when the probe was written. Unscoped, the count
	// answers a question about the whole database and the premise fires on a table
	// this test has never touched.
	var present int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_schema = current_schema() AND table_name='workspaces' AND column_name='compression_policy'`,
	).Scan(&present); err != nil {
		t.Fatalf("premise probe: %v", err)
	}
	if present != 0 {
		t.Fatalf("premise: compression_policy is still on the table (%d) — this test is not measuring the migration", present)
	}

	// A workspace that predates the column. It never consented to anything.
	if _, err := pool.Exec(ctx,
		`INSERT INTO workspaces (id, name, cache_prefix) VALUES ('mig-existing','E','p:')`); err != nil {
		t.Fatalf("seed the pre-0117 row: %v", err)
	}

	sql, err := os.ReadFile("../../migrations/0117_workspace_compression_policy.sql")
	if err != nil {
		t.Fatalf("read the migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("apply 0117: %v", err)
	}

	var existing string
	if err := pool.QueryRow(ctx,
		`SELECT compression_policy FROM workspaces WHERE id='mig-existing'`).Scan(&existing); err != nil {
		t.Fatalf("read the migrated row: %v", err)
	}
	if existing != "disabled" {
		t.Errorf("an EXISTING workspace came out of 0117 as %q, want \"disabled\" — nobody opted into the rewriter, so nobody keeps it by inheritance", existing)
	}

	// And a row written after the migration that omits the column.
	if _, err := pool.Exec(ctx,
		`INSERT INTO workspaces (id, name, cache_prefix) VALUES ('mig-after','A','p:')`); err != nil {
		t.Fatalf("insert a post-migration row: %v", err)
	}
	var fresh string
	if err := pool.QueryRow(ctx,
		`SELECT compression_policy FROM workspaces WHERE id='mig-after'`).Scan(&fresh); err != nil {
		t.Fatalf("read the post-migration row: %v", err)
	}
	if fresh != "disabled" {
		t.Errorf("a row inserted after 0117 that omits the column came out %q, want \"disabled\"", fresh)
	}

	// CI applies every migration twice and requires the second run to be a no-op.
	// ADD COLUMN IF NOT EXISTS is what makes that true here; assert it rather than
	// leaving it to the CI job to discover.
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Errorf("0117 is not re-runnable, which the CI migrate-rerun step requires: %v", err)
	}
}

// AN OPT-IN MUST SURVIVE A RESTART, AND THE DEFAULT MUST SURVIVE IT TOO. The hot
// path reads the policy from the in-memory map (GetCompressionPolicy), and that
// map is rebuilt from scratch by LoadAll at boot. This drives the whole round
// trip on a real row: register → persisted → Set → persisted → a FRESH Manager
// over the same database → the same answer.
func TestCompressionPolicy_SurvivesRestart_RealPG(t *testing.T) {
	pool := restartTestPool(t)
	ctx := context.Background()
	mA := New(pool)

	if err := mA.RegisterWorkspace(ctx, Workspace{
		ID: "ws-on", Name: "On", Active: true, CompressionPolicy: CompressionAlways,
	}); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}
	var stored string
	if err := pool.QueryRow(ctx,
		`SELECT compression_policy FROM workspaces WHERE id='ws-on'`).Scan(&stored); err != nil {
		t.Fatalf("read back after register: %v", err)
	}
	if stored != "always" {
		t.Errorf("registration persisted compression_policy=%q, want \"always\" — the opt-in only lived in the cache", stored)
	}

	if err := mA.SetCompressionPolicy(ctx, "ws-on", CompressionOptIn); err != nil {
		t.Fatalf("SetCompressionPolicy: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT compression_policy FROM workspaces WHERE id='ws-on'`).Scan(&stored); err != nil {
		t.Fatalf("read back after set: %v", err)
	}
	if stored != "opt_in" {
		t.Errorf("SetCompressionPolicy persisted %q, want \"opt_in\"", stored)
	}

	// A workspace that set nothing at all — the case every workspace is in today.
	if err := mA.RegisterWorkspace(ctx, Workspace{ID: "ws-plain", Name: "Plain", Active: true}); err != nil {
		t.Fatalf("RegisterWorkspace(plain): %v", err)
	}

	// THE RESTART. A fresh Manager over the same database, through the boot path.
	mB := New(pool)
	if err := mB.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if got := mB.GetCompressionPolicy("ws-on"); got != CompressionOptIn {
		t.Errorf("after a restart the opt-in read back as %q, want opt_in — LoadAll is not carrying the column", got)
	}
	if got := mB.GetCompressionPolicy("ws-plain"); got != CompressionDisabled {
		t.Errorf("after a restart a workspace that set nothing read back as %q, want disabled", got)
	}
}

// RE-REGISTRATION IS THE DIRECTION THAT MATTERS. `insertWorkspaceSQL`'s ON
// CONFLICT list includes compression_policy, so a blind re-POST that omits the
// field decodes to "" → normalizes to the default → writes 'disabled'. That is
// asymmetric on purpose and the asymmetry is the safety argument: a blind
// re-registration can REVOKE an opt-in (loudly — prompts stop being rewritten)
// and can never GRANT one. Pinned in both directions, because only the second
// half is a security property and only the first half is surprising.
func TestCompressionPolicy_BlindReRegistrationRevokesNeverGrants_RealPG(t *testing.T) {
	pool := restartTestPool(t)
	ctx := context.Background()
	m := New(pool)

	if err := m.RegisterWorkspace(ctx, Workspace{
		ID: "ws-re", Name: "Re", Active: true, CompressionPolicy: CompressionAlways,
	}); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}
	// A blind re-POST with no compression field at all.
	if err := m.RegisterWorkspace(ctx, Workspace{ID: "ws-re", Name: "Re", Active: true}); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	var stored string
	if err := pool.QueryRow(ctx,
		`SELECT compression_policy FROM workspaces WHERE id='ws-re'`).Scan(&stored); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored != "disabled" {
		t.Errorf("a blind re-registration left compression_policy=%q; it must fall back to the default", stored)
	}

	// The direction that would be a defect: a re-POST cannot turn the rewriter ON
	// for a workspace that is off, no matter what an unrecognised value says.
	if err := m.RegisterWorkspace(ctx, Workspace{
		ID: "ws-off", Name: "Off", Active: true, CompressionPolicy: CompressionDisabled,
	}); err != nil {
		t.Fatalf("RegisterWorkspace(off): %v", err)
	}
	if err := m.RegisterWorkspace(ctx, Workspace{
		ID: "ws-off", Name: "Off", Active: true, CompressionPolicy: CompressionPolicy("alway"),
	}); err != nil {
		t.Fatalf("re-register(off): %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT compression_policy FROM workspaces WHERE id='ws-off'`).Scan(&stored); err != nil {
		t.Fatalf("read back(off): %v", err)
	}
	if stored != "disabled" {
		t.Errorf("a near-miss typo re-registered a disabled workspace as %q — a typo must never start rewriting prompts", stored)
	}
}
