package outputverify_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/outputverify"
	"github.com/talyvor/lens/migrations"
)

var ovMigrateOnce sync.Once

func ovTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG outputverify test")
	}
	ctx := context.Background()
	ovMigrateOnce.Do(func() {
		conn, err := pgx.Connect(ctx, url)
		if err != nil {
			t.Fatalf("connect for migrate: %v", err)
		}
		defer conn.Close(ctx)
		if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
			t.Fatalf("apply migrations: %v", err)
		}
	})
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func rec(outputID, wsID, verdict, reason, kind string) outputverify.VerdictRecord {
	return outputverify.VerdictRecord{
		OutputID: outputID, WorkspaceID: wsID, Model: "openai/gpt-4o",
		Verdict: verdict, Reason: reason, ConstraintKind: kind,
		PromptSHA256: outputverify.Sha256Hex([]byte("p:" + outputID)), ResponseSHA256: outputverify.Sha256Hex([]byte("r:" + outputID)),
	}
}

// APPEND-ONLY DEDUP: re-recording the same output_id is a no-op (replay/re-serve safe).
func TestBreach_AppendOnlyDedup(t *testing.T) {
	ctx := context.Background()
	pool := ovTestPool(t)
	w := outputverify.NewWriter(pool)
	r := rec("oid-dedup-1", "wsD", outputverify.VerdictFailed, outputverify.ReasonInvalidJSON, outputverify.KindJSONObject)
	if ins, err := w.Record(ctx, r); err != nil || !ins {
		t.Fatalf("first Record ins=%v err=%v", ins, err)
	}
	if ins, err := w.Record(ctx, r); err != nil || ins {
		t.Errorf("second Record must dedup (ON CONFLICT output_id); ins=%v err=%v", ins, err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM k4_output_verdicts WHERE output_id=$1`, "oid-dedup-1").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("append-only: exactly 1 row, got %d", n)
	}
}

// HASHES ONLY + SELF ONLY: the table has NO raw prompt/response text column and NO counterparty column.
func TestBreach_NoRawContent_NoCounterparty(t *testing.T) {
	ctx := context.Background()
	pool := ovTestPool(t)
	_ = outputverify.NewWriter(pool) // ensure migrated
	rows, err := pool.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_name='k4_output_verdicts'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	allowed := map[string]bool{
		"output_id": true, "workspace_id": true, "model": true, "verdict": true,
		"reason": true, "constraint_kind": true, "prompt_sha256": true, "response_sha256": true, "created_at": true,
	}
	forbidden := []string{"prompt_text", "response_text", "content", "counterparty", "other_workspace", "raw"}
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			t.Fatal(err)
		}
		if !allowed[col] {
			t.Errorf("unexpected column %q — the schema must carry hashes only, self only", col)
		}
		for _, bad := range forbidden {
			if strings.Contains(col, bad) {
				t.Errorf("column %q looks like raw content / a counterparty — forbidden", col)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// WORKSPACE-SCOPED READ: a tenant reads ONLY its own verdicts; never another's.
func TestBreach_WorkspaceScopedRead(t *testing.T) {
	ctx := context.Background()
	pool := ovTestPool(t)
	w := outputverify.NewWriter(pool)
	if _, err := w.Record(ctx, rec("oid-A1", "ws-alpha", outputverify.VerdictPassed, "", outputverify.KindJSONObject)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Record(ctx, rec("oid-A2", "ws-alpha", outputverify.VerdictUnverifiable, "", outputverify.KindNone)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Record(ctx, rec("oid-B1", "ws-beta", outputverify.VerdictFailed, outputverify.ReasonSchemaViolation, outputverify.KindJSONSchema)); err != nil {
		t.Fatal(err)
	}
	got, err := outputverify.NewReader(pool).ListForWorkspace(ctx, "ws-alpha", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("expected ws-alpha's own verdicts")
	}
	for _, v := range got {
		if v.WorkspaceID != "ws-alpha" {
			t.Errorf("workspace-scoped read leaked a foreign workspace: %q", v.WorkspaceID)
		}
	}
}

// The verdict ENUM is SQL-ENFORCED (a bond can never be built on a bogus verdict; unverifiable is a real,
// distinct value). A raw insert of an out-of-enum verdict must be rejected by the CHECK constraint.
func TestBreach_VerdictEnumEnforced(t *testing.T) {
	ctx := context.Background()
	pool := ovTestPool(t)
	_ = outputverify.NewWriter(pool)
	_, err := pool.Exec(ctx, `INSERT INTO k4_output_verdicts
        (output_id, workspace_id, model, verdict, prompt_sha256, response_sha256)
        VALUES ('oid-bad','wsE','m','slashable_maybe','ph','rh')`)
	if err == nil {
		t.Fatal("an out-of-enum verdict must be rejected by the CHECK constraint")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "check") && !strings.Contains(strings.ToLower(err.Error()), "constraint") {
		t.Errorf("want a CHECK-constraint rejection; got %v", err)
	}
	// And the three real values are all accepted (incl. unverifiable — structurally distinct from failed).
	w := outputverify.NewWriter(pool)
	for i, v := range []string{outputverify.VerdictPassed, outputverify.VerdictFailed, outputverify.VerdictUnverifiable} {
		if _, err := w.Record(ctx, rec("oid-enum-"+string(rune('a'+i)), "wsE", v, "", outputverify.KindNone)); err != nil {
			t.Errorf("verdict %q must be accepted: %v", v, err)
		}
	}
}
