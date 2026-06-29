package poolroyalty

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMain isolates this package's LENS_TEST_DATABASE_URL-gated integration tests in a UNIQUE private
// schema (search_path = lens_it_poolroyalty, public). Without it, the parallel `go test ./...` package binaries
// race on shared PUBLIC-schema tables (lens_token_balances, lxc_purchases, the 0001 types, …) — a
// PRE-EXISTING flaky failure reproduced on cc30050, independent of any feature PR. Test-only: it changes
// no production code. A harness that sets its OWN search_path via pgxpool RuntimeParams overrides this and
// keeps its own schema. `public` stays in the path so the shared (idempotent) vector extension resolves;
// tables are created in the private schema (first in the path), so no cross-package collision remains.
// No-op when the env var is unset (the gated tests self-skip).
func TestMain(m *testing.M) {
	if base := os.Getenv("LENS_TEST_DATABASE_URL"); base != "" {
		ctx := context.Background()
		if admin, err := pgxpool.New(ctx, base); err == nil {
			// Serialize cross-package setup (CREATE EXTENSION / schema DDL) under one advisory lock so
			// concurrent package TestMains can't race on the shared extension/catalog.
			if tx, terr := admin.Begin(ctx); terr == nil {
				_, _ = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(727274)")
				_, _ = tx.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector")
				_, _ = tx.Exec(ctx, "DROP SCHEMA IF EXISTS lens_it_poolroyalty CASCADE")
				_, _ = tx.Exec(ctx, "CREATE SCHEMA lens_it_poolroyalty")
				_ = tx.Commit(ctx)
			}
			admin.Close()
			sep := "?"
			if strings.Contains(base, "?") {
				sep = "&"
			}
			os.Setenv("LENS_TEST_DATABASE_URL", base+sep+"options="+url.QueryEscape("-c search_path=lens_it_poolroyalty,public"))
		}
	}
	os.Exit(m.Run())
}
