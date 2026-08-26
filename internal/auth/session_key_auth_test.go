package auth

// session_key_auth_test.go — W4.6.1 STEP 4: what a session key authorises, and what it cannot.
//
// ⚠ THE WHOLE POINT OF THIS FILE IS THE SECOND HALF. It is easy to prove a new credential WORKS;
// the property that makes a session key worth building is everything it CANNOT do. So every test
// that admits it is paired with the gates that refuse it, and the scope set is asserted by VALUE
// rather than by "contains proxy" — a set that merely contains proxy could also contain admin.
//
// ⚠ AND THE SCOPE SET IS A CONSTANT IN THIS PACKAGE, NOT A COLUMN. internal/sessionkey has no
// scopes field at all. That is deliberate and it is the direct lesson of
// docs/model2-session-credentials.md: the neighbouring credential takes its scopes from the request
// body through a route with no gate, so a caller can mint a credential carrying scopes it does not
// itself hold. There is nowhere to put a scope on a session key, so that cannot recur.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/sessionkey"
)

const skAuthSchema = "lens_it_sesskeyauth"

func skAuthPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG session-key auth test")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = skAuthSchema
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), `
		DROP SCHEMA IF EXISTS `+skAuthSchema+` CASCADE;
		CREATE SCHEMA `+skAuthSchema+`;
		CREATE TABLE `+skAuthSchema+`.session_keys (
			id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			workspace_id TEXT NOT NULL,
			user_id      TEXT NOT NULL,
			key_hash     TEXT NOT NULL,
			key_prefix   TEXT NOT NULL,
			expires_at   TIMESTAMPTZ NOT NULL,
			created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			last_used_at TIMESTAMPTZ
		);
		CREATE UNIQUE INDEX ON `+skAuthSchema+`.session_keys (key_hash);
		CREATE TABLE `+skAuthSchema+`.api_keys (
			id TEXT PRIMARY KEY, key_hash TEXT UNIQUE NOT NULL,
			workspace_id TEXT NOT NULL DEFAULT 'default', team TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL DEFAULT '', active BOOLEAN NOT NULL DEFAULT true,
			created_at TIMESTAMPTZ DEFAULT NOW(), last_used_at TIMESTAMPTZ, expires_at TIMESTAMPTZ);
	`); err != nil {
		t.Fatalf("build fixture schema: %v", err)
	}
	return pool
}

func skAuthManager(t *testing.T, pool *pgxpool.Pool) (*Manager, *sessionkey.Store) {
	t.Helper()
	store := sessionkey.NewStore(pool)
	return NewManager("skauth-global-admin", nil, New(pool), nil).WithSessionKeys(store), store
}

func skSigningKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	return k
}

func skRequest(raw string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", http.NoBody)
	r.Header.Set("Authorization", "Bearer "+raw)
	return r
}

// ⚠ THE HEADLINE, AND IT IS AN EQUALITY. A session key resolves to EXACTLY {proxy}.
func TestSessionKey_AuthenticatesWithExactlyTheProxyScope(t *testing.T) {
	pool := skAuthPool(t)
	m, store := skAuthManager(t, pool)

	raw, _, err := store.Mint(context.Background(), "ws-sk", "user-sk", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	actx, err := m.Authenticate(skRequest(raw))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if want := []string{ScopeProxy}; !reflect.DeepEqual(actx.Scopes, want) {
		t.Fatalf("scopes = %v, want EXACTLY %v. This is asserted by value on purpose: a set that "+
			"merely CONTAINS proxy could also contain admin, and the whole case for this credential "+
			"is that it carries one capability", actx.Scopes, want)
	}
	if actx.IsAdmin {
		t.Fatal("a session key authenticated as ADMIN — IsAdmin short-circuits HasScope, so this " +
			"credential would satisfy every gate in the binary")
	}
	if actx.AuthMethod != MethodSessionKey {
		t.Fatalf("AuthMethod = %q, want %q — \"which credential spent this balance\" is exactly the "+
			"question an incident asks, and a shared method name cannot answer it",
			actx.AuthMethod, MethodSessionKey)
	}
	if actx.WorkspaceID != "ws-sk" || actx.UserID != "user-sk" {
		t.Fatalf("resolved (%q,%q), want (ws-sk,user-sk) — a session key resolving to the wrong "+
			"owner spends someone else's balance", actx.WorkspaceID, actx.UserID)
	}
	if actx.APIKeyID != "" {
		t.Fatalf("APIKeyID = %q, want empty. It keys the F4 per-agent LXC sub-budget allocator; a "+
			"session key is not a scoped workspace key and must not enter that path", actx.APIKeyID)
	}
}

// ⚠ THE REFUSALS ARE THE PRODUCT. Admitting the proxy gate is worth nothing unless every other gate
// refuses — and RequireScope's grandfather admits an EMPTY scope set to EVERYTHING, so a session
// key whose scopes failed to populate would pass all four of these.
func TestSessionKey_PassesTheProxyGateAndNothingElse(t *testing.T) {
	pool := skAuthPool(t)
	m, store := skAuthManager(t, pool)
	raw, _, err := store.Mint(context.Background(), "ws-gate", "user-gate", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	for _, tc := range []struct {
		scope string
		want  int
	}{
		{ScopeProxy, http.StatusOK},
		{ScopeAnalytics, http.StatusForbidden},
		{ScopeAdmin, http.StatusForbidden},
		{ScopeKeys, http.StatusForbidden},
		{ScopeMint, http.StatusForbidden},
		{ScopeOperatorRead, http.StatusForbidden},
	} {
		h := AuthMiddleware(New(pool), m)(RequireScope(tc.scope)(http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, skRequest(raw))
		if rec.Code != tc.want {
			t.Errorf("RequireScope(%q) admitted/refused a session key with %d, want %d (%s)",
				tc.scope, rec.Code, tc.want, rec.Body.String())
		}
	}
}

func TestSessionKey_ExpiredIsRefused(t *testing.T) {
	pool := skAuthPool(t)
	m, store := skAuthManager(t, pool)
	raw, _, err := store.Mint(context.Background(), "ws-exp", "user-exp", -time.Minute)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := m.Authenticate(skRequest(raw)); err == nil {
		t.Fatal("an EXPIRED session key authenticated — the expiry is the only thing making this a " +
			"session credential rather than a workspace one")
	}
}

// Same shape as internal/tenant's revocation test, and for the same reason: authenticate FIRST so a
// cache would be warm if one existed, then revoke, then assert the VERY NEXT call fails.
func TestSessionKey_RevokedIsRefusedOnTheVeryNextRequest(t *testing.T) {
	pool := skAuthPool(t)
	m, store := skAuthManager(t, pool)
	ctx := context.Background()
	raw, _, err := store.Mint(ctx, "ws-rev", "user-rev", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := m.Authenticate(skRequest(raw)); err != nil {
		t.Fatalf("the key must work BEFORE revocation or this proves nothing: %v", err)
	}
	if _, err := m.Authenticate(skRequest(raw)); err != nil {
		t.Fatalf("second pre-revocation call: %v", err)
	}
	if _, err := store.RevokeAll(ctx, "ws-rev", "user-rev"); err != nil {
		t.Fatalf("RevokeAll: %v", err)
	}
	if _, err := m.Authenticate(skRequest(raw)); err == nil {
		t.Fatal("a revoked session key still authenticates — sign-out does not end the session")
	}
}

// ⚠ A PREFIX COLLISION WOULD SEND ONE CREDENTIAL SHAPE TO THE WRONG STORE, and the failure would be
// a silent lookup miss (a valid key refused) rather than an error anyone reads.
func TestSessionKeyPrefixIsDisjointFromTheWorkspaceKeyPrefix(t *testing.T) {
	const wsPrefix = "tlv_ws_" // tenant.KeyPrefix, restated here because internal/tenant imports would cycle
	if strings.HasPrefix(sessionkey.KeyPrefix, wsPrefix) || strings.HasPrefix(wsPrefix, sessionkey.KeyPrefix) {
		t.Fatalf("session-key prefix %q and workspace-key prefix %q are not disjoint",
			sessionkey.KeyPrefix, wsPrefix)
	}
}

// ⚠ THE CLAMP THE HTTP LAYER NEEDS. A session key must never outlive the session that asked for it,
// and the mint handler can only enforce that if the caller's own expiry reaches it.
func TestJWTAuthContextCarriesItsExpiry(t *testing.T) {
	pool := skAuthPool(t)
	m, _ := skAuthManager(t, pool)
	pk := skSigningKey(t)
	m.privateKey, m.publicKey = pk, &pk.PublicKey

	tok, err := GenerateToken("ws-exp-claim", "user-exp-claim", []string{ScopeAnalytics}, pk, 90*time.Minute)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	actx, err := m.Authenticate(skRequest(tok))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if actx.ExpiresAt.IsZero() {
		t.Fatal("a JWT AuthContext carries no ExpiresAt — the mint handler cannot clamp a session " +
			"key to the life of the session, so the key would outlive the sign-in that created it")
	}
	if d := time.Until(actx.ExpiresAt); d < 80*time.Minute || d > 95*time.Minute {
		t.Fatalf("ExpiresAt is %v away, want ~90m — the claim is not the token's own exp", d)
	}
}
