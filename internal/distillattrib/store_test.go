package distillattrib

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fakeExecer records the single Exec the store is allowed to make.
type fakeExecer struct {
	sql  string
	args []any
	err  error
}

func (f *fakeExecer) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.sql = sql
	f.args = args
	return pgconn.CommandTag{}, f.err
}

// TestRecordDistillServe_UpsertShape pins the write: an UPSERT-increment against
// the attribution table — and MINT-FREE, the SQL can never reference a ledger.
func TestRecordDistillServe_UpsertShape(t *testing.T) {
	fe := &fakeExecer{}
	s := NewStore(fe)
	if err := s.RecordDistillServe(context.Background(), "wsA", "wsB", "h1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fe.sql, "INSERT INTO distill_serve_attribution") {
		t.Fatalf("SQL = %q, want INSERT INTO distill_serve_attribution", fe.sql)
	}
	// MINT-FREE (structural): the only write this store emits is the attribution
	// counter — never a ledger / spend / royalty / LXC table.
	for _, banned := range []string{"lens_token_ledger", "token_events", "pool_royalty", "lxc_", "credit"} {
		if strings.Contains(strings.ToLower(fe.sql), banned) {
			t.Fatalf("MINT-FREE VIOLATION: store SQL references %q:\n%s", banned, fe.sql)
		}
	}
	// Re-serve bumps the count (idempotent on the tuple), never appends.
	if !strings.Contains(fe.sql, "ON CONFLICT") || !strings.Contains(fe.sql, "distill_serve_attribution.serve_count + 1") {
		t.Fatalf("SQL missing the ON CONFLICT serve_count increment:\n%s", fe.sql)
	}
	if len(fe.args) != 3 || fe.args[0] != "wsA" || fe.args[1] != "wsB" || fe.args[2] != "h1" {
		t.Fatalf("args = %v, want [wsA wsB h1]", fe.args)
	}
}

func TestNewStore_NilPoolInert(t *testing.T) {
	if NewStore(nil) != nil {
		t.Fatal("NewStore(nil) must be nil (attribution inert)")
	}
	if err := (*Store)(nil).RecordDistillServe(context.Background(), "a", "b", "h"); err != nil {
		t.Fatalf("nil store must no-op, got %v", err)
	}
}

// TestRecordDistillServe_IncrementAndIdempotent_Integration proves the UPSERT
// against real Postgres: re-serving the same (owner, requester, hash) bumps
// serve_count on ONE row; a different requester is a new row. Gated on
// LENS_TEST_DATABASE_URL (runs in CI's real-PG job, skipped locally).
func TestRecordDistillServe_IncrementAndIdempotent_Integration(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set LENS_TEST_DATABASE_URL for the real-PG attribution test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	t.Cleanup(pool.Close)
	// Mirror migration 0052 on a clean table.
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS distill_serve_attribution`,
		`CREATE TABLE distill_serve_attribution (
			owner_workspace_id TEXT NOT NULL, requester_workspace_id TEXT NOT NULL,
			content_hash TEXT NOT NULL, serve_count BIGINT NOT NULL DEFAULT 0,
			first_served_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			last_served_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (owner_workspace_id, requester_workspace_id, content_hash))`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("ddl: %v", err)
		}
	}
	s := NewStore(pool)

	// Two serves of the same tuple → serve_count=2 on ONE row.
	if err := s.RecordDistillServe(ctx, "wsA", "wsB", "h1"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordDistillServe(ctx, "wsA", "wsB", "h1"); err != nil {
		t.Fatal(err)
	}
	var count, rows int64
	if err := pool.QueryRow(ctx, `SELECT serve_count, (SELECT COUNT(*) FROM distill_serve_attribution)
		FROM distill_serve_attribution WHERE owner_workspace_id='wsA' AND requester_workspace_id='wsB' AND content_hash='h1'`).
		Scan(&count, &rows); err != nil {
		t.Fatal(err)
	}
	if count != 2 || rows != 1 {
		t.Fatalf("after two same-tuple serves: serve_count=%d rows=%d, want 2 and 1", count, rows)
	}
	// A different requester is a SEPARATE row.
	if err := s.RecordDistillServe(ctx, "wsA", "wsC", "h1"); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM distill_serve_attribution`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("after a new requester: rows=%d, want 2", rows)
	}
}
