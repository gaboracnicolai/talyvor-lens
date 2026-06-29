package routingpredict

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMain isolates this package's LENS_TEST_DATABASE_URL-gated integration tests in a UNIQUE private
// schema (search_path = lens_it_routingpredict, public), mirroring the #251 isolation pattern. Without it,
// the parallel `go test ./...` package binaries race on shared public-schema tables. Test-only. `public`
// stays in the path for the shared idempotent vector extension; tables land in the private schema (first
// in the path). No-op when the env var is unset (the gated tests self-skip).
func TestMain(m *testing.M) {
	if base := os.Getenv("LENS_TEST_DATABASE_URL"); base != "" {
		ctx := context.Background()
		if admin, err := pgxpool.New(ctx, base); err == nil {
			if tx, terr := admin.Begin(ctx); terr == nil {
				_, _ = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(727274)")
				_, _ = tx.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector")
				_, _ = tx.Exec(ctx, "DROP SCHEMA IF EXISTS lens_it_routingpredict CASCADE")
				_, _ = tx.Exec(ctx, "CREATE SCHEMA lens_it_routingpredict")
				_ = tx.Commit(ctx)
			}
			admin.Close()
			sep := "?"
			if strings.Contains(base, "?") {
				sep = "&"
			}
			os.Setenv("LENS_TEST_DATABASE_URL", base+sep+"options="+url.QueryEscape("-c search_path=lens_it_routingpredict,public"))
		}
	}
	os.Exit(m.Run())
}
