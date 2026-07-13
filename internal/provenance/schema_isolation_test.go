package provenance

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMain isolates this package's LENS_TEST_DATABASE_URL-gated integration tests in a UNIQUE private schema
// (search_path = lens_it_provenance, public) — the peer harness the other DB-backed packages ship. Without
// it, the parallel `go test ./...` package binaries race on shared PUBLIC-schema tables (and a prior
// package's partial migration breaks dbmigrate — the failure that silently disabled keel's breach suite).
// Test-only. `public` stays in the path so the shared vector extension resolves; tables are created in the
// private schema. No-op when the env var is unset.
func TestMain(m *testing.M) {
	if base := os.Getenv("LENS_TEST_DATABASE_URL"); base != "" {
		ctx := context.Background()
		if admin, err := pgxpool.New(ctx, base); err == nil {
			if tx, terr := admin.Begin(ctx); terr == nil {
				_, _ = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(727274)")
				_, _ = tx.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector")
				_, _ = tx.Exec(ctx, "DROP SCHEMA IF EXISTS lens_it_provenance CASCADE")
				_, _ = tx.Exec(ctx, "CREATE SCHEMA lens_it_provenance")
				_ = tx.Commit(ctx)
			}
			admin.Close()
			sep := "?"
			if strings.Contains(base, "?") {
				sep = "&"
			}
			os.Setenv("LENS_TEST_DATABASE_URL", base+sep+"options="+url.QueryEscape("-c search_path=lens_it_provenance,public"))
		}
	}
	os.Exit(m.Run())
}
