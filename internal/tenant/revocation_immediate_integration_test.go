package tenant_test

// REVOCATION IS IMMEDIATE — pinned against a real Postgres, because the claim is a security property
// and the only honest evidence is the behaviour of the real validator against the real schema.
//
// ⚠ WHY THIS TEST EXISTS AT ALL, AND WHAT IT CORRECTS. A previous audit (mine) reported that a
// revoked workspace key keeps working for up to five minutes, reasoning from auth.KeyStore's
// cacheTTL = 5 * time.Minute in internal/auth/apikeys.go. That was WRONG, and wrong in the
// frightening direction: it told operators a working security control was broken.
//
// There are TWO key systems in Lens and they share almost every word:
//
//   · auth.KeyStore    → table `api_keys`            → CACHED, 5-minute TTL, purged by
//                                                       auth.KeyStore.Revoke
//   · tenant.Store     → table `workspace_api_keys`  → NEVER cached; every request hits the DB,
//                                                       revoked by tenant.Store.RevokeAPIKey
//
// The workspace keys a customer mints are the SECOND kind. auth/manager.go says so in as many
// words — "Workspace key — always validate against the DB, never cached, so a revoked key stops
// working immediately" — and ValidateAPIKey below proves it by querying every time.
//
// ⚠ SO THE FIX THAT WAS ASKED FOR WOULD HAVE BEEN A SEVERE REGRESSION. Swapping the endpoint to
// auth.KeyStore.Revoke would run `UPDATE api_keys SET active = false WHERE id = $1` against a
// workspace key's id — a different table, zero rows matched — and the credential would never be
// revoked at all, while the endpoint returned success. The reason to write this test rather than
// make that change is the reason to keep it: the property is real, and nothing was asserting it.
//
// The shape of the test is the one the brief demanded: authenticate FIRST, so a cache would be warm
// if one existed, then revoke, then assert the VERY NEXT validation fails. A test that revokes a key
// the process has never validated proves nothing — it is exactly the shape that lets a caching bug
// ship.

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/tenant"
)

// revokePool gives the test its OWN schema and its own tables.
//
// ⚠ IT DOES NOT USE THE MIGRATED public SCHEMA, and that is not fastidiousness. The shared lens_test
// database is torn down and rebuilt by other packages in the same `-p 1` run — internal/keel and
// internal/localrouter DROP SCHEMA outright — so a test that assumes migrations are still applied
// passes alone and fails in CI depending on package order. That is exactly how this test first
// failed, and the fixture is the fix rather than a retry.
//
// The cost is that these two tables are described here as well as in migrations/. That drift is
// LOUD (an insert against a changed schema fails immediately) and it is the same trade every other
// real-Postgres test in this repo already makes — see internal/routedecision and internal/keel.
const revokeSchema = "lens_it_revoke"

func revokePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG revocation test")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	// Every pooled connection resolves unqualified table names to this test's schema, so
	// tenant.Store's own SQL runs unmodified against the fixture.
	cfg.ConnConfig.RuntimeParams["search_path"] = revokeSchema
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		DROP SCHEMA IF EXISTS `+revokeSchema+` CASCADE;
		CREATE SCHEMA `+revokeSchema+`;
		CREATE TABLE `+revokeSchema+`.workspaces (
			id           TEXT PRIMARY KEY,
			name         TEXT NOT NULL,
			cache_prefix TEXT NOT NULL
		);
		CREATE TABLE `+revokeSchema+`.workspace_api_keys (
			id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			workspace_id TEXT NOT NULL REFERENCES `+revokeSchema+`.workspaces(id),
			key_hash     TEXT NOT NULL,
			key_prefix   TEXT NOT NULL,
			name         TEXT NOT NULL,
			scopes       TEXT[] NOT NULL,
			last_used_at TIMESTAMPTZ,
			expires_at   TIMESTAMPTZ,
			created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
	`); err != nil {
		t.Fatalf("build fixture schema: %v", err)
	}
	return pool
}

// seedWorkspace creates an isolated workspace and returns its id.
func seedWorkspace(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $2, $3)`, id, id, id,
	); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	return id
}

// TestRevokedWorkspaceKeyIsRefusedOnTheVERYNextRequest is the whole claim.
func TestRevokedWorkspaceKeyIsRefusedOnTheVERYNextRequest(t *testing.T) {
	pool := revokePool(t)
	store := tenant.NewStore(pool)
	ctx := context.Background()
	ws := seedWorkspace(t, pool, "revoke-immediate-ws")

	raw, key, err := store.CreateAPIKey(ctx, ws, "probe", []string{"proxy"}, nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	// 1. AUTHENTICATE FIRST. This is the step the naive version of this test omits: it is what
	//    would populate a verification cache, and without it "revoked keys are refused" can pass
	//    while a cached key sails through in production.
	if _, err := store.ValidateAPIKey(ctx, raw); err != nil {
		t.Fatalf("the key must work BEFORE revocation, or the test proves nothing: %v", err)
	}
	// Validate a second time so a lazily-populated or refresh-on-read cache is certainly warm.
	if _, err := store.ValidateAPIKey(ctx, raw); err != nil {
		t.Fatalf("second pre-revocation validation failed: %v", err)
	}

	// 2. REVOKE — through the exact function the HTTP endpoint calls.
	if err := store.RevokeAPIKey(ctx, key.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// 3. THE VERY NEXT REQUEST. No sleep, no TTL wait — if this passes only after five minutes,
	//    it fails here, which is the entire point.
	if _, err := store.ValidateAPIKey(ctx, raw); !errors.Is(err, tenant.ErrInvalidKey) {
		t.Fatalf("a revoked key was still accepted immediately after revocation: err=%v — "+
			"revocation is not taking effect, or a verification cache has been introduced on this path", err)
	}
}

// TestRevokeIsIdempotentAndScopedToOneKey: revoking twice is not an error (the endpoint may be
// retried), and it takes exactly one key with it — a revoke that quietly widened to the workspace
// would be catastrophic and would otherwise look like success.
func TestRevokeIsIdempotentAndScopedToOneKey(t *testing.T) {
	pool := revokePool(t)
	store := tenant.NewStore(pool)
	ctx := context.Background()
	ws := seedWorkspace(t, pool, "revoke-scope-ws")

	rawA, keyA, err := store.CreateAPIKey(ctx, ws, "a", []string{"proxy"}, nil)
	if err != nil {
		t.Fatalf("mint a: %v", err)
	}
	rawB, _, err := store.CreateAPIKey(ctx, ws, "b", []string{"proxy"}, nil)
	if err != nil {
		t.Fatalf("mint b: %v", err)
	}
	if _, err := store.ValidateAPIKey(ctx, rawB); err != nil {
		t.Fatalf("key b must work before: %v", err)
	}

	if err := store.RevokeAPIKey(ctx, keyA.ID); err != nil {
		t.Fatalf("revoke a: %v", err)
	}
	if err := store.RevokeAPIKey(ctx, keyA.ID); err != nil {
		t.Fatalf("second revoke must be idempotent, got: %v", err)
	}

	if _, err := store.ValidateAPIKey(ctx, rawA); !errors.Is(err, tenant.ErrInvalidKey) {
		t.Fatalf("key a survived revocation: %v", err)
	}
	if _, err := store.ValidateAPIKey(ctx, rawB); err != nil {
		t.Fatalf("key b was taken down by another key's revocation: %v", err)
	}
}
