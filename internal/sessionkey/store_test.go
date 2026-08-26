package sessionkey_test

// store_test.go — W4.6.1 STEP 4: SESSION-SCOPED KEYS, so a browser chat never holds a workspace key.
//
// ⚠ WHAT A SESSION KEY IS, AND WHAT IT IS NOT. It is a credential bound to (workspace, user) that
// ALWAYS expires and that a caller cannot describe. Compare the credential a browser session can
// obtain today (measured in cmd/lens/session_credential_reach_test.go and
// docs/model2-session-credentials.md — a workspace API key carrying caller-chosen scopes):
//
//                        workspace API key            session key
//   lifetime             none unless asked for        ALWAYS set, NOT NULL in the schema
//   scopes               chosen by the caller         not stored at all; fixed in auth
//   blast radius         the whole workspace          the (workspace, user) pair
//   survives sign-out    yes                          no — RevokeAll is what sign-out calls
//
// ⚠ THE SCOPE SET IS DELIBERATELY ABSENT FROM THIS PACKAGE. A scope column is a scope a row can
// carry, and a scope a row can carry is one a caller can eventually be persuaded to set — which is
// precisely the defect measured on the workspace-key mint route, where a credential can mint a
// credential carrying scopes it does not itself hold. What a session key authorises is a constant
// in internal/auth, next to the gate that reads it, and internal/auth/session_key_auth_test.go is
// where that is asserted.
//
// Real Postgres because every claim here is about the behaviour of the real validator against the
// real schema — an in-memory double would be asserting my own arithmetic.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/sessionkey"
)

// skSchema — this test's own schema, for the reason internal/tenant's revocation test records:
// other packages in the same `-p 1` run DROP SCHEMA outright, so a test that assumes migrations are
// still applied passes alone and fails on package order.
const skSchema = "lens_it_sesskey"

func skPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG session-key store test")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = skSchema
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// The DDL mirrors migrations/0122_session_keys.sql. ⚠ expires_at is NOT NULL here for the same
	// reason it is NOT NULL there: "a session key always expires" is the property that makes it a
	// SESSION key, and a property enforced by the column cannot be forgotten by a caller.
	if _, err := pool.Exec(context.Background(), `
		DROP SCHEMA IF EXISTS `+skSchema+` CASCADE;
		CREATE SCHEMA `+skSchema+`;
		CREATE TABLE `+skSchema+`.session_keys (
			id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			workspace_id TEXT NOT NULL,
			user_id      TEXT NOT NULL,
			key_hash     TEXT NOT NULL,
			key_prefix   TEXT NOT NULL,
			expires_at   TIMESTAMPTZ NOT NULL,
			created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			last_used_at TIMESTAMPTZ
		);
		CREATE UNIQUE INDEX ON `+skSchema+`.session_keys (key_hash);
		CREATE INDEX ON `+skSchema+`.session_keys (workspace_id, user_id);
	`); err != nil {
		t.Fatalf("build fixture schema: %v", err)
	}
	return pool
}

func TestMint_ReturnsAUsableKeyAndStoresNoPlaintext(t *testing.T) {
	pool := skPool(t)
	store := sessionkey.NewStore(pool)
	ctx := context.Background()

	raw, k, err := store.Mint(ctx, "ws-mint", "user-mint", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !strings.HasPrefix(raw, sessionkey.KeyPrefix) {
		t.Fatalf("minted key %q does not carry the %q prefix — Manager.Authenticate dispatches on "+
			"it, so a key without it can never be recognised", raw, sessionkey.KeyPrefix)
	}
	if k.ExpiresAt.IsZero() {
		t.Fatal("minted key has a zero expiry — a session key that never expires is a workspace key")
	}

	// ⚠ THE PLAINTEXT MUST NOT BE IN THE ROW. Read the stored hash back through SQL rather than
	// trusting the struct: the struct is what the code returns, the row is what an attacker who
	// reads the database gets.
	var hash string
	if err := pool.QueryRow(ctx, `SELECT key_hash FROM session_keys WHERE id = $1`, k.ID).Scan(&hash); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if hash == raw || strings.Contains(hash, strings.TrimPrefix(raw, sessionkey.KeyPrefix)) {
		t.Fatal("the stored key_hash contains the plaintext key — a database read is a credential dump")
	}

	got, err := store.Validate(ctx, raw)
	if err != nil {
		t.Fatalf("Validate on a freshly minted key: %v", err)
	}
	if got.WorkspaceID != "ws-mint" || got.UserID != "user-mint" {
		t.Fatalf("Validate resolved (%q,%q), want (ws-mint,user-mint) — a session key that resolves "+
			"to the wrong owner spends someone else's balance", got.WorkspaceID, got.UserID)
	}
}

func TestValidate_RefusesAnExpiredKey(t *testing.T) {
	pool := skPool(t)
	store := sessionkey.NewStore(pool)
	ctx := context.Background()

	// A negative TTL is the honest way to reach the expired branch without sleeping: the row is
	// written with expires_at in the past, which is exactly the state a key reaches on its own.
	raw, _, err := store.Mint(ctx, "ws-exp", "user-exp", -time.Minute)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := store.Validate(ctx, raw); !errors.Is(err, sessionkey.ErrExpired) {
		t.Fatalf("Validate on an expired key = %v, want ErrExpired. An expiry the validator does "+
			"not enforce is a comment, not a lifetime", err)
	}
}

func TestValidate_RefusesGarbageAndForeignPrefixes(t *testing.T) {
	pool := skPool(t)
	store := sessionkey.NewStore(pool)
	ctx := context.Background()
	for _, raw := range []string{
		"",
		"nonsense",
		"tlv_ws_0123456789abcdef", // a WORKSPACE key shape must not resolve here
		sessionkey.KeyPrefix + "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef", // right shape, never minted
	} {
		if _, err := store.Validate(ctx, raw); err == nil {
			t.Fatalf("Validate(%q) succeeded — it must not", raw)
		}
	}
}

// ⚠ THE SHAPE OF THIS TEST IS THE POINT: authenticate FIRST, so a cache would be warm if one
// existed, THEN revoke, then assert the VERY NEXT validation fails. A test that revokes a key the
// process has never validated proves nothing — it is exactly the shape that lets a caching bug
// ship. internal/tenant's revocation test records why that matters here.
func TestRevokeAll_IsWhatSignOutCalls_AndTakesEffectImmediately(t *testing.T) {
	pool := skPool(t)
	store := sessionkey.NewStore(pool)
	ctx := context.Background()

	rawA, _, err := store.Mint(ctx, "ws-signout", "user-1", time.Hour)
	if err != nil {
		t.Fatalf("Mint A: %v", err)
	}
	rawB, _, err := store.Mint(ctx, "ws-signout", "user-1", time.Hour)
	if err != nil {
		t.Fatalf("Mint B: %v", err)
	}
	// A DIFFERENT user in the SAME workspace. Sign-out is per USER; revoking a colleague's live
	// chat because someone else signed out is a bug that looks like a security feature.
	rawOther, _, err := store.Mint(ctx, "ws-signout", "user-2", time.Hour)
	if err != nil {
		t.Fatalf("Mint other: %v", err)
	}

	for _, raw := range []string{rawA, rawB, rawOther} {
		if _, err := store.Validate(ctx, raw); err != nil {
			t.Fatalf("a key must work BEFORE revocation or this test proves nothing: %v", err)
		}
	}
	// Second pass so a lazily-populated or refresh-on-read cache is certainly warm.
	if _, err := store.Validate(ctx, rawA); err != nil {
		t.Fatalf("second pre-revocation validation: %v", err)
	}

	n, err := store.RevokeAll(ctx, "ws-signout", "user-1")
	if err != nil {
		t.Fatalf("RevokeAll: %v", err)
	}
	if n != 2 {
		t.Fatalf("RevokeAll removed %d keys, want 2 — the count is what a sign-out handler logs, "+
			"and a wrong count hides a key that survived", n)
	}
	for i, raw := range []string{rawA, rawB} {
		if _, err := store.Validate(ctx, raw); err == nil {
			t.Fatalf("key %d still validates after sign-out — the session outlived the session", i)
		}
	}
	if _, err := store.Validate(ctx, rawOther); err != nil {
		t.Fatalf("user-2's key was revoked by user-1 signing out: %v", err)
	}
}

// ⚠ REVOKE IS WORKSPACE-SCOPED BY ARGUMENT, NOT BY CONVENTION. Revoking by id alone would let any
// caller who learns an id revoke another tenant's live session — the same shape the workspace-key
// delete route had to close by checking ownership before revoking.
func TestRevoke_WillNotReachAnotherWorkspacesKey(t *testing.T) {
	pool := skPool(t)
	store := sessionkey.NewStore(pool)
	ctx := context.Background()

	rawVictim, victim, err := store.Mint(ctx, "ws-victim", "user-v", time.Hour)
	if err != nil {
		t.Fatalf("Mint victim: %v", err)
	}
	if err := store.Revoke(ctx, "ws-attacker", victim.ID); err != nil {
		t.Fatalf("Revoke naming a foreign workspace should be a silent no-op, got: %v", err)
	}
	if _, err := store.Validate(ctx, rawVictim); err != nil {
		t.Fatalf("another workspace revoked this key by id: %v", err)
	}
	if err := store.Revoke(ctx, "ws-victim", victim.ID); err != nil {
		t.Fatalf("the owning workspace could not revoke its own key: %v", err)
	}
	if _, err := store.Validate(ctx, rawVictim); err == nil {
		t.Fatal("the owner revoked the key and it still validates")
	}
}
