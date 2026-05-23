package prompts

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func newPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestCreate_InsertsPromptWithVersion1(t *testing.T) {
	pool := newPool(t)
	pool.ExpectExec(`INSERT INTO prompts`).
		WithArgs(pgxmock.AnyArg(), "my-prompt", 1, "hello", "desc", "ws-1", true, "alice").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	m := newManager(pool)
	p, err := m.Create(context.Background(), Prompt{
		Name: "my-prompt", Content: "hello", Description: "desc",
		WorkspaceID: "ws-1", CreatedBy: "alice",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if p.Version != 1 {
		t.Errorf("Version = %d, want 1", p.Version)
	}
	if !p.IsActive {
		t.Errorf("IsActive = false, want true on new prompt")
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestUpdate_CreatesVersion2AndDeactivatesVersion1(t *testing.T) {
	pool := newPool(t)
	// Create v1 (INSERT).
	pool.ExpectExec(`INSERT INTO prompts`).WithArgs(
		pgxmock.AnyArg(), "my-prompt", 1, "v1 content", pgxmock.AnyArg(), "ws-1", true, pgxmock.AnyArg(),
	).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	// Update: Get hits cache, then UPDATE (deactivate) + INSERT (v2).
	pool.ExpectExec(`UPDATE prompts SET is_active = false`).WithArgs("my-prompt", "ws-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	pool.ExpectExec(`INSERT INTO prompts`).WithArgs(
		pgxmock.AnyArg(), "my-prompt", 2, "v2 content", pgxmock.AnyArg(), "ws-1", true, pgxmock.AnyArg(),
	).WillReturnResult(pgxmock.NewResult("INSERT", 1))

	m := newManager(pool)
	if _, err := m.Create(context.Background(), Prompt{
		Name: "my-prompt", Content: "v1 content", WorkspaceID: "ws-1",
	}); err != nil {
		t.Fatalf("Create v1: %v", err)
	}
	v2, err := m.Update(context.Background(), "my-prompt", "ws-1", "v2 content", "updated")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if v2.Version != 2 {
		t.Errorf("Version = %d, want 2", v2.Version)
	}
}

func TestGet_ReturnsActiveVersionFromCache(t *testing.T) {
	pool := newPool(t)
	pool.ExpectExec(`INSERT INTO prompts`).WithArgs(
		pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
		pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
	).WillReturnResult(pgxmock.NewResult("INSERT", 1))

	m := newManager(pool)
	created, _ := m.Create(context.Background(), Prompt{
		Name: "cached", Content: "hi", WorkspaceID: "ws-1",
	})

	got, err := m.Get(context.Background(), "cached", "ws-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || got.ID != created.ID {
		t.Errorf("Get did not return cached prompt; got %+v want %+v", got, created)
	}
	// pgxmock has only the Create INSERT expectation — if Get touched the
	// DB, ExpectationsWereMet would still pass but a SELECT would have
	// errored upstream. Belt + suspenders.
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestGetVersion_ReturnsSpecificVersion(t *testing.T) {
	pool := newPool(t)
	now := time.Now().UTC()
	pool.ExpectQuery(`FROM prompts WHERE name = \$1 AND workspace_id = \$2 AND version = \$3`).
		WithArgs("my-prompt", "ws-1", 5).
		WillReturnRows(
			pgxmock.NewRows([]string{
				"id", "name", "version", "content", "description",
				"workspace_id", "is_active", "created_by", "created_at", "updated_at",
			}).AddRow("uuid-5", "my-prompt", 5, "v5 content", "v5 desc", "ws-1", false, "alice", now, now),
		)

	m := newManager(pool)
	got, err := m.GetVersion(context.Background(), "my-prompt", "ws-1", 5)
	if err != nil {
		t.Fatalf("GetVersion: %v", err)
	}
	if got == nil || got.Version != 5 || got.Content != "v5 content" {
		t.Errorf("GetVersion returned wrong record: %+v", got)
	}
}

func TestRollback_CreatesNewVersionWithOldContent(t *testing.T) {
	pool := newPool(t)
	// Create v1 (INSERT)
	pool.ExpectExec(`INSERT INTO prompts`).WithArgs(
		pgxmock.AnyArg(), "p", 1, "v1", pgxmock.AnyArg(), "ws-1", true, pgxmock.AnyArg(),
	).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	// Update to v2 (UPDATE + INSERT)
	pool.ExpectExec(`UPDATE prompts SET is_active = false`).WithArgs("p", "ws-1").WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	pool.ExpectExec(`INSERT INTO prompts`).WithArgs(
		pgxmock.AnyArg(), "p", 2, "v2", pgxmock.AnyArg(), "ws-1", true, pgxmock.AnyArg(),
	).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	// Rollback: GetVersion(1) → SELECT, then Update → UPDATE + INSERT
	now := time.Now().UTC()
	pool.ExpectQuery(`FROM prompts WHERE name = \$1 AND workspace_id = \$2 AND version = \$3`).
		WithArgs("p", "ws-1", 1).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "name", "version", "content", "description",
			"workspace_id", "is_active", "created_by", "created_at", "updated_at",
		}).AddRow("uuid-1", "p", 1, "v1", "", "ws-1", false, "", now, now))
	pool.ExpectExec(`UPDATE prompts SET is_active = false`).WithArgs("p", "ws-1").WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	pool.ExpectExec(`INSERT INTO prompts`).WithArgs(
		pgxmock.AnyArg(), "p", 3, "v1", pgxmock.AnyArg(), "ws-1", true, pgxmock.AnyArg(),
	).WillReturnResult(pgxmock.NewResult("INSERT", 1))

	m := newManager(pool)
	ctx := context.Background()
	if _, err := m.Create(ctx, Prompt{Name: "p", Content: "v1", WorkspaceID: "ws-1"}); err != nil {
		t.Fatalf("Create v1: %v", err)
	}
	if _, err := m.Update(ctx, "p", "ws-1", "v2", ""); err != nil {
		t.Fatalf("Update v2: %v", err)
	}
	rb, err := m.Rollback(ctx, "p", "ws-1", 1)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if rb.Version != 3 {
		t.Errorf("rollback Version = %d, want 3 (continuity of version numbers)", rb.Version)
	}
	if rb.Content != "v1" {
		t.Errorf("rollback Content = %q, want v1", rb.Content)
	}
}

func TestHistory_ReturnsAllVersionsOrderedByVersionDesc(t *testing.T) {
	pool := newPool(t)
	now := time.Now().UTC()
	pool.ExpectQuery(`FROM prompts WHERE name = \$1 AND workspace_id = \$2 ORDER BY version DESC`).
		WithArgs("p", "ws-1").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "name", "version", "content", "description",
			"workspace_id", "is_active", "created_by", "created_at", "updated_at",
		}).
			AddRow("u3", "p", 3, "v3", "", "ws-1", true, "", now, now).
			AddRow("u2", "p", 2, "v2", "", "ws-1", false, "", now, now).
			AddRow("u1", "p", 1, "v1", "", "ws-1", false, "", now, now))

	m := newManager(pool)
	rows, err := m.History(context.Background(), "p", "ws-1")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d versions, want 3", len(rows))
	}
	if rows[0].Version != 3 || rows[2].Version != 1 {
		t.Errorf("order wrong: %+v", []int{rows[0].Version, rows[1].Version, rows[2].Version})
	}
}

func TestDiff_CountsAddedAndRemovedLines(t *testing.T) {
	m := newManager(nil)
	m.seedForTest(&Prompt{Name: "p", Version: 1, WorkspaceID: "ws-1", Content: "line A\nline B\nline C"})
	m.seedForTest(&Prompt{Name: "p", Version: 2, WorkspaceID: "ws-1", Content: "line A\nline B PRIME\nline C\nline D"})

	d, err := m.Diff(context.Background(), "p", "ws-1", 1, 2)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	// "line B" removed, "line B PRIME" + "line D" added.
	if d.Added != 2 {
		t.Errorf("Added = %d, want 2", d.Added)
	}
	if d.Removed != 1 {
		t.Errorf("Removed = %d, want 1", d.Removed)
	}
	if d.FromVersion != 1 || d.ToVersion != 2 {
		t.Errorf("version metadata wrong: from=%d to=%d", d.FromVersion, d.ToVersion)
	}
	if !strings.Contains(d.Diff, "+line B PRIME") {
		t.Errorf("diff missing added line marker:\n%s", d.Diff)
	}
	if !strings.Contains(d.Diff, "-line B") {
		t.Errorf("diff missing removed line marker:\n%s", d.Diff)
	}
}

func TestResolve_ReplacesLensPromptReference(t *testing.T) {
	m := newManager(nil)
	m.seedForTest(&Prompt{
		Name: "greeting", Version: 1, WorkspaceID: "ws-1", IsActive: true,
		Content: "Hello from the prompt manager.",
	})

	body := []byte(`{"model":"gpt-4","messages":[{"role":"system","content":"lens:prompt:greeting"},{"role":"user","content":"hi"}]}`)
	out, err := m.Resolve(context.Background(), body, "ws-1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	msgs := got["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["content"] != "Hello from the prompt manager." {
		t.Errorf("system content not resolved; got %v", first["content"])
	}
}

func TestResolve_ReturnsBodyUnchangedWhenNoReference(t *testing.T) {
	m := newManager(nil)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"just a normal prompt"}]}`)

	out, err := m.Resolve(context.Background(), body, "ws-1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("Resolve mutated body without lens:prompt reference:\n in: %s\nout: %s", body, out)
	}
}

func TestList_ReturnsOnlyActivePrompts(t *testing.T) {
	pool := newPool(t)
	now := time.Now().UTC()
	pool.ExpectQuery(`FROM prompts WHERE workspace_id = \$1 AND is_active = true`).
		WithArgs("ws-1").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "name", "version", "content", "description",
			"workspace_id", "is_active", "created_by", "created_at", "updated_at",
		}).
			AddRow("u1", "alpha", 3, "alpha-v3", "", "ws-1", true, "", now, now).
			AddRow("u2", "beta", 1, "beta-v1", "", "ws-1", true, "", now, now))

	m := newManager(pool)
	rows, err := m.List(context.Background(), "ws-1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d prompts, want 2", len(rows))
	}
	for _, p := range rows {
		if !p.IsActive {
			t.Errorf("inactive prompt leaked into List: %+v", p)
		}
	}
}
