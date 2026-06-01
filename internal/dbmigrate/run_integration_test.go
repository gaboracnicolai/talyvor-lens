package dbmigrate

import (
	"context"
	"os"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
)

// connectTestDB skips the test unless LENS_TEST_DATABASE_URL points at a
// throwaway Postgres. Keeps the default unit suite hermetic (no DB needed)
// while letting CI / a local docker drill exercise the real runner.
func connectTestDB(t *testing.T) *pgx.Conn {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping DB integration test")
	}
	conn, err := pgx.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("connect test DB: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	// Clean slate so reruns of the test itself are deterministic.
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS schema_migrations`,
		`DROP TABLE IF EXISTS dbmigrate_t1`,
		`DROP TABLE IF EXISTS dbmigrate_t2`,
	} {
		if _, err := conn.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("reset: %v", err)
		}
	}
	return conn
}

func TestRun_AppliesAllThenIsIdempotent(t *testing.T) {
	conn := connectTestDB(t)
	ctx := context.Background()
	fsys := fstest.MapFS{
		"0001_t1.sql": {Data: []byte(`CREATE TABLE dbmigrate_t1 (id int)`)},
		"0002_t2.sql": {Data: []byte(`CREATE TABLE dbmigrate_t2 (id int)`)},
	}

	first, err := Run(ctx, conn, fsys)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("first Run applied %v, want 2 migrations", first)
	}

	// Re-run is a no-op — the tracking table makes it idempotent even though
	// bare CREATE TABLE would otherwise fail on the second pass.
	second, err := Run(ctx, conn, fsys)
	if err != nil {
		t.Fatalf("second Run must not error (idempotent): %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("second Run applied %v, want 0 (already applied)", second)
	}
}

func TestRun_FailsLoudAndDoesNotRecordOnError(t *testing.T) {
	conn := connectTestDB(t)
	ctx := context.Background()
	fsys := fstest.MapFS{
		"0001_ok.sql":  {Data: []byte(`CREATE TABLE dbmigrate_t1 (id int)`)},
		"0002_bad.sql": {Data: []byte(`THIS IS NOT VALID SQL;`)},
	}

	applied, err := Run(ctx, conn, fsys)
	if err == nil {
		t.Fatal("a broken migration must return a non-nil error (Job must fail loudly)")
	}
	// 0001 applied + recorded; 0002 failed and must NOT be recorded, so a
	// fixed re-run would retry exactly 0002.
	if len(applied) != 1 || applied[0] != "0001" {
		t.Fatalf("applied = %v, want [0001] (only the good one committed)", applied)
	}
	var count int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version = '0002'`).Scan(&count); err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 0 {
		t.Fatal("failed migration 0002 must not be recorded in schema_migrations")
	}
}
