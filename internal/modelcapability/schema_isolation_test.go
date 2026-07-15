package modelcapability

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMain isolates this package's LENS_TEST_DATABASE_URL-gated integration tests
// in a UNIQUE private schema (search_path = lens_it_modelcapability, public) so the
// `go test ./...` package binaries never race on shared PUBLIC-schema tables. Same
// pattern as internal/worktier's isolation. Test-only; no production code changes.
// No-op when the env var is unset (the gated tests self-skip).
func TestMain(m *testing.M) {
	if base := os.Getenv("LENS_TEST_DATABASE_URL"); base != "" {
		ctx := context.Background()
		if admin, err := pgxpool.New(ctx, base); err == nil {
			if tx, terr := admin.Begin(ctx); terr == nil {
				_, _ = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(727295)")
				_, _ = tx.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector")
				_, _ = tx.Exec(ctx, "DROP SCHEMA IF EXISTS lens_it_modelcapability CASCADE")
				_, _ = tx.Exec(ctx, "CREATE SCHEMA lens_it_modelcapability")
				_ = tx.Commit(ctx)
			}
			admin.Close()
			sep := "?"
			if strings.Contains(base, "?") {
				sep = "&"
			}
			os.Setenv("LENS_TEST_DATABASE_URL", base+sep+"options="+url.QueryEscape("-c search_path=lens_it_modelcapability,public"))
		}
	}
	os.Exit(m.Run())
}
