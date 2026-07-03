package auth

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/talyvor/lens/internal/tenant"
)

// F4-capstone step C.0 — AuthContext.APIKeyID threading. API-key auth carries the scoped key's ID (the
// C.1 allocator keys the per-agent LXC sub-budget on it); every non-API-key path carries "".

// (proof 2 + 3-JWT) THE LOAD-BEARING NEGATIVE: a JWT must carry NO API-key ID (else it would wrongly qualify
// for the agent-allocation path in C.1), and its existing fields are unchanged.
func TestAuthContext_JWTCarriesNoAPIKeyID(t *testing.T) {
	key := testKey(t)
	mgr := NewManager("", key, nil, nil)
	tok, _ := GenerateToken("ws_a", "user_a", []string{ScopeProxy}, key, time.Hour)
	ctx, err := mgr.Authenticate(newReq(tok))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if ctx.APIKeyID != "" {
		t.Fatalf("JWT auth must carry EMPTY APIKeyID (JWT traffic must not enter the agent path), got %q", ctx.APIKeyID)
	}
	// WorkspaceID/UserID/Scopes/AuthMethod byte-for-byte unchanged (this PR only ADDS a field).
	if ctx.WorkspaceID != "ws_a" || ctx.UserID != "user_a" || ctx.AuthMethod != MethodJWT ||
		len(ctx.Scopes) != 1 || ctx.Scopes[0] != ScopeProxy {
		t.Fatalf("JWT auth fields changed: %+v", ctx)
	}
}

// (proof 3, global-admin negative) the global admin key also carries no key ID.
func TestAuthContext_GlobalKeyCarriesNoAPIKeyID(t *testing.T) {
	mgr := NewManager("super-secret-admin", nil, nil, nil)
	ctx, err := mgr.Authenticate(newReq("super-secret-admin"))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if ctx.APIKeyID != "" {
		t.Fatalf("global-admin must carry EMPTY APIKeyID, got %q", ctx.APIKeyID)
	}
}

// (proof 1 + 3-APIkey) API-KEY AUTH CARRIES THE ID: a valid workspace API key ⇒ APIKeyID == that key's ID
// (non-empty, matches), and WorkspaceID resolution is unchanged. Real PG (the workspace-key branch runs the
// tenant store's DB validate — no mock seam reachable from package auth).
func TestAuthContext_APIKeyCarriesID(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG API-key auth test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS workspace_api_keys (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL, key_hash TEXT NOT NULL,
			key_prefix TEXT NOT NULL, name TEXT NOT NULL DEFAULT '', scopes TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[],
			last_used_at TIMESTAMPTZ, expires_at TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE INDEX IF NOT EXISTS workspace_api_keys_prefix_idx ON workspace_api_keys (key_prefix)`,
		`TRUNCATE workspace_api_keys`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}

	store := tenant.NewStore(pool)
	raw, meta, err := store.CreateAPIKey(ctx, "ws_agent", "agent-key", []string{"proxy"}, nil)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if meta.ID == "" {
		t.Fatal("created key must have a non-empty ID")
	}

	mgr := NewManager("", nil, nil, store)
	actx, err := mgr.Authenticate(newReq(raw))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	// (proof 1) APIKeyID == the created key's ID, non-empty.
	if actx.APIKeyID == "" || actx.APIKeyID != meta.ID {
		t.Fatalf("API-key auth must carry the key ID: got %q, want %q", actx.APIKeyID, meta.ID)
	}
	// (proof 3-APIkey) WorkspaceID/AuthMethod unchanged.
	if actx.WorkspaceID != "ws_agent" || actx.AuthMethod != MethodWorkspaceKey {
		t.Fatalf("API-key auth fields changed: %+v", actx)
	}
}
