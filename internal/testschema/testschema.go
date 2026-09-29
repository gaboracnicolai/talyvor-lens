// Package testschema gives one package's LENS_TEST_DATABASE_URL-gated tests a private schema on the
// shared test database, so a builder's plain `go test ./...` — package binaries in parallel, one
// database — behaves like CI's serialised `-p 1` run on a fresh one.
//
// Without it, packages that create, drop or purge a table in PUBLIC collide with every other package
// using that table (cmd/lens recreating pool_royalty_mints under internal/earnings; token_events seeded
// under internal/api and internal/learner), and on a fresh database the first test to run 0001_init.sql
// from a scratch search_path strands the pgvector `vector` type in that scratch schema, so every later
// migration fails with `type "vector" does not exist`.
//
// The internal/*/schema_isolation_test.go harnesses predate this package and do the same inline.
package testschema

import (
	"context"
	"errors"
	"log"
	"net/url"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
)

// setupLock is the advisory-lock key every isolation harness in this repo serialises its setup under,
// so concurrent package binaries never race on the database-global extension or the catalog.
const setupLock = 727274

// Isolate is called from a package's TestMain before m.Run. With LENS_TEST_DATABASE_URL set it
// creates the pgvector extension in PUBLIC, recreates the package's private schema empty, and rewrites
// LENS_TEST_DATABASE_URL so every connection built from it resolves names in (schema, public). A test
// that sets its own search_path through RuntimeParams keeps it. Unset: a no-op, and the gated tests skip.
func Isolate(schema string) {
	base := os.Getenv("LENS_TEST_DATABASE_URL")
	if base == "" {
		return
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		log.Fatalf("testschema: connect to LENS_TEST_DATABASE_URL: %v", err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		log.Fatalf("testschema: begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, setupLock); err != nil {
		log.Fatalf("testschema: advisory lock: %v", err)
	}
	// A database a stranded run already broke is healed rather than left failing: pgvector is
	// relocatable, so the extension is moved back to public if it sits anywhere else.
	var home string
	err = tx.QueryRow(ctx, `SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname = 'vector'`).Scan(&home)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		log.Fatalf("testschema: look up the vector extension: %v", err)
	}
	stmts := []string{`CREATE EXTENSION IF NOT EXISTS vector SCHEMA public`}
	if home != "" && home != "public" {
		stmts = []string{`ALTER EXTENSION vector SET SCHEMA public`}
	}
	stmts = append(stmts, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`, `CREATE SCHEMA `+schema)
	for _, stmt := range stmts {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			log.Fatalf("testschema: %s: %v", stmt, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		log.Fatalf("testschema: commit: %v", err)
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	os.Setenv("LENS_TEST_DATABASE_URL", base+sep+"options="+url.QueryEscape("-c search_path="+schema+",public"))
}
