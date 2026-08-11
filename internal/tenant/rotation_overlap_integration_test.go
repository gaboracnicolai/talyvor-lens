package tenant_test

// ROTATING A KEY A RUNNING SERVICE IS HOLDING — measured against a real Postgres, because the
// question is about the ORDER two states become visible to a third party, and no unit test of a
// single function can see that.
//
// ⚠ THE FINDING THIS FILE PINS: the endpoint named `rotate` is the ONE path that cannot be used to
// rotate a credential a live process holds, and its own source comment reads as though it can.
// RotateAPIKey wraps SELECT FOR UPDATE -> INSERT new -> DELETE old in a SINGLE transaction, and
// says:
//
//	"The INSERT precedes the DELETE so no window exists where zero active keys exist for the
//	 workspace (the old key is live until the new one is safely persisted)."
//
// That sentence is TRUE ABOUT THE TRANSACTION AND FALSE ABOUT THE SYSTEM. Ordering INSERT before
// DELETE is invisible outside the transaction: both rows change visibility at COMMIT, atomically.
// So there is no instant at which a CLIENT can authenticate with the old key and the new key both
// — the old credential dies the moment the call returns. Any service still holding it is locked
// out for the whole of "write the new key to config -> restart the process", and for Talyvor's BFF
// that interval is every login.
//
// ⚠ W1.9 STATES THE REQUIRED ORDER AND IT IS NOT EXPRESSIBLE THROUGH RotateAPIKey: "mint the new
// key -> write it to /etc/talyvor/bff.env -> restart talyvor-bff -> VERIFY A REAL LOGIN -> only
// then delete the old row." That order needs a window in which BOTH keys authenticate. Only
// CreateAPIKey + RevokeAPIKey, two separate calls, produce one.
//
// ⚠ SO THE ANSWER TO "IS THERE A SUPPORTED WAY TO ROTATE A LEAKED KEY" IS YES, BUT NOT THE OBVIOUS
// ONE, and picking the obvious one causes the outage the item warns about. Both tests below are
// necessary: the first alone would read as "rotation is broken", the second alone as "rotation is
// fine". The pair is the finding.
//
// ⚠ NEITHER TEST ASSERTS THAT RotateAPIKey IS WRONG. Atomic replace is the correct primitive for a
// key held by a human who can paste the new value immediately, and its transaction is genuinely
// race-free — that is what the TOCTOU comment above it is about. What is measured here is that it
// is the wrong primitive for a key held by a PROCESS, which is a property of the caller, not a bug
// in the function.

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/tenant"
)

// ⚠ ITS OWN SCHEMA, NOT revocation_immediate_integration_test.go's. That file explains why the
// shared migrated `public` schema cannot be relied on (internal/keel and internal/localrouter DROP
// SCHEMA in the same `-p 1` run). The same reasoning applies here, and a second file sharing one
// fixture schema would couple two independent findings to one teardown.
const rotateSchema = "lens_it_rotate"

func rotatePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG rotation-overlap test")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = rotateSchema
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(context.Background(), `
		DROP SCHEMA IF EXISTS `+rotateSchema+` CASCADE;
		CREATE SCHEMA `+rotateSchema+`;
		CREATE TABLE `+rotateSchema+`.workspaces (
			id           TEXT PRIMARY KEY,
			name         TEXT NOT NULL,
			cache_prefix TEXT NOT NULL
		);
		CREATE TABLE `+rotateSchema+`.workspace_api_keys (
			id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			workspace_id TEXT NOT NULL REFERENCES `+rotateSchema+`.workspaces(id),
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

func seedRotateWorkspace(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $2, $3)`, id, id, id,
	); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	return id
}

// TestRotateAPIKeyLeavesNoOverlapWindowForALiveHolder is the half that says why the BFF cannot be
// rotated with the rotate endpoint.
func TestRotateAPIKeyLeavesNoOverlapWindowForALiveHolder(t *testing.T) {
	pool := rotatePool(t)
	store := tenant.NewStore(pool)
	ctx := context.Background()
	ws := seedRotateWorkspace(t, pool, "rotate-overlap-ws")

	// The credential a running service already holds — the BFF's `tlv_ws_...` in bff.env.
	oldRaw, oldKey, err := store.CreateAPIKey(ctx, ws, "bff", []string{"proxy"}, nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := store.ValidateAPIKey(ctx, oldRaw); err != nil {
		t.Fatalf("the held key must authenticate BEFORE rotation or this test proves nothing: %v", err)
	}

	newRaw, fresh, err := store.RotateAPIKey(ctx, ws, oldKey.ID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}

	// The new key works — so the rotation did happen and a failure below is about the OLD key,
	// not about a broken fixture.
	if _, err := store.ValidateAPIKey(ctx, newRaw); err != nil {
		t.Fatalf("the freshly rotated key does not authenticate: %v", err)
	}
	if fresh.ID == oldKey.ID {
		t.Fatal("rotate returned the same key id — no new credential was issued")
	}

	// ⚠ THE MEASUREMENT. If the old key still authenticated here there WOULD be an overlap window
	// and the BFF could be rotated with one call. It does not.
	if _, err := store.ValidateAPIKey(ctx, oldRaw); !errors.Is(err, tenant.ErrInvalidKey) {
		t.Fatalf("the OLD key still authenticates after RotateAPIKey (err=%v) — if this is the "+
			"behaviour now, an overlap window exists and this file's finding is stale: re-measure "+
			"before trusting the runbook", err)
	}
	t.Log("MEASURED: after RotateAPIKey the old credential is refused immediately — a process " +
		"holding it is locked out from commit until it is restarted with the new value")
}

// TestCreateThenRevokeIsTheOnlyPathWithAnOverlapWindow is the half that says what to do instead.
//
// ⚠ WITHOUT THIS TEST THE ONE ABOVE READS AS "ROTATION IS BROKEN", which is false and is the more
// damaging error: it would send someone to build a second rotation mechanism that already exists.
func TestCreateThenRevokeIsTheOnlyPathWithAnOverlapWindow(t *testing.T) {
	pool := rotatePool(t)
	store := tenant.NewStore(pool)
	ctx := context.Background()
	ws := seedRotateWorkspace(t, pool, "rotate-overlap-ws")

	oldRaw, oldKey, err := store.CreateAPIKey(ctx, ws, "bff", []string{"proxy"}, nil)
	if err != nil {
		t.Fatalf("mint old: %v", err)
	}

	// STEP 1 — mint a SECOND key. The schema has no unique constraint on workspace_id
	// (migrations/0018_tenants.sql), so two live keys for one workspace is a supported state and
	// not an accident this test is exploiting.
	newRaw, newKey, err := store.CreateAPIKey(ctx, ws, "bff-rotated", []string{"proxy"}, nil)
	if err != nil {
		t.Fatalf("mint new: %v", err)
	}
	if newKey.ID == oldKey.ID {
		t.Fatal("the second mint returned the first key")
	}

	// ⚠ THE WINDOW. Both credentials authenticate at the same time. This is the state W1.9's
	// ordering requires and the state RotateAPIKey never produces: the service keeps working on
	// the old value while the new one is written to config and the process is restarted.
	if _, err := store.ValidateAPIKey(ctx, oldRaw); err != nil {
		t.Fatalf("the OLD key stopped working as soon as a second was minted (%v) — there is then "+
			"NO safe rotation path at all and the runbook is wrong", err)
	}
	if _, err := store.ValidateAPIKey(ctx, newRaw); err != nil {
		t.Fatalf("the NEW key does not authenticate while the old one lives: %v", err)
	}

	// STEP 2 — only after the restart is verified does the old row go. Revocation is immediate
	// (revocation_immediate_integration_test.go pins that separately); asserted here because the
	// window has to CLOSE, or "rotation" has just left two live credentials for a leaked key.
	if err := store.RevokeAPIKey(ctx, oldKey.ID); err != nil {
		t.Fatalf("revoke old: %v", err)
	}
	if _, err := store.ValidateAPIKey(ctx, oldRaw); !errors.Is(err, tenant.ErrInvalidKey) {
		t.Fatalf("the leaked key still authenticates after revocation: %v", err)
	}
	if _, err := store.ValidateAPIKey(ctx, newRaw); err != nil {
		t.Fatalf("revoking the old key took the new one with it: %v", err)
	}
	t.Log("MEASURED: CreateAPIKey then RevokeAPIKey gives an overlap window in which BOTH " +
		"credentials authenticate, and closes it cleanly — this is the supported rotation order")
}
