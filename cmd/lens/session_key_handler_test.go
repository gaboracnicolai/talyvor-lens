package main

// session_key_handler_test.go — W4.6.1 STEP 4, THE HTTP BOUNDARY.
//
// ⚠ THE MINT ROUTE IS THE PART THAT HAD TO NOT REPEAT THE DEFECT NEXT DOOR.
// docs/model2-session-credentials.md measures the workspace-key mint route: it has no scope gate,
// and it takes the new credential's scopes from the request body — so a credential can mint a
// credential carrying scopes it does not itself hold. This route is the same *shape* of operation
// (a credential asks for a credential), so the refusals below are not defensive extras; they are
// the reason the route is safe to add at all:
//
//   · WHO may mint    — exactly one credential shape: a browser SESSION JWT. Everything else 403s,
//                       INCLUDING a session key itself, so the credential cannot re-mint or extend
//                       itself into an unbounded chain.
//   · WHAT it grants  — not expressible in the request. There is no scopes field to send.
//   · HOW LONG        — clamped to the caller's OWN remaining life, so a key cannot outlive the
//                       sign-in that created it even if the caller asks for a year.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/sessionkey"
	"github.com/talyvor/lens/internal/tenant"
)

const skHandlerSchema = "lens_it_skhandler"

func skHandlerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG session-key handler test")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = skHandlerSchema
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), `
		DROP SCHEMA IF EXISTS `+skHandlerSchema+` CASCADE;
		CREATE SCHEMA `+skHandlerSchema+`;
		CREATE TABLE `+skHandlerSchema+`.session_keys (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL,
			user_id TEXT NOT NULL, key_hash TEXT NOT NULL, key_prefix TEXT NOT NULL,
			expires_at TIMESTAMPTZ NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			last_used_at TIMESTAMPTZ);
		CREATE UNIQUE INDEX ON `+skHandlerSchema+`.session_keys (key_hash);
		CREATE TABLE `+skHandlerSchema+`.workspace_api_keys (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL,
			key_hash TEXT NOT NULL, key_prefix TEXT NOT NULL, name TEXT NOT NULL DEFAULT '',
			scopes TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[], last_used_at TIMESTAMPTZ,
			expires_at TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW());
		CREATE INDEX ON `+skHandlerSchema+`.workspace_api_keys (key_prefix);
		CREATE TABLE `+skHandlerSchema+`.api_keys (
			id TEXT PRIMARY KEY, key_hash TEXT UNIQUE NOT NULL,
			workspace_id TEXT NOT NULL DEFAULT 'default', team TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL DEFAULT '', active BOOLEAN NOT NULL DEFAULT true,
			created_at TIMESTAMPTZ DEFAULT NOW(), last_used_at TIMESTAMPTZ, expires_at TIMESTAMPTZ);
	`); err != nil {
		t.Fatalf("build fixture schema: %v", err)
	}
	return pool
}

type skFixture struct {
	router  http.Handler
	mgr     *auth.Manager
	store   *sessionkey.Store
	tenants *tenant.Store
	pk      *ecdsa.PrivateKey
}

// skRouter composes the routes the way main.go does — r.Group, not chi.NewRouter().Use.
//
// ⚠ THE DIFFERENCE IS LOAD-BEARING and it is documented at length in
// cmd/lens/session_credential_reach_test.go: a Use middleware on a top-level Mux runs BEFORE the
// route is matched, so chi.URLParam returns "" inside it. A control caught a fixture that got this
// wrong and proved nothing as a result.
func skRouter(t *testing.T, pool *pgxpool.Pool, ttl time.Duration) *skFixture {
	t.Helper()
	pk, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	store := sessionkey.NewStore(pool)
	tenants := tenant.NewStore(pool)
	mgr := auth.NewManager("skhandler-global-admin", pk, auth.New(pool), tenants).WithSessionKeys(store)

	r := chi.NewRouter()
	r.Group(func(authed chi.Router) {
		authed.Use(auth.AuthMiddleware(auth.New(pool), mgr))
		mountSessionKeyRoutes(authed, store, ttl)
	})
	return &skFixture{router: r, mgr: mgr, store: store, tenants: tenants, pk: pk}
}

func (f *skFixture) sessionJWT(t *testing.T, ws, user string, life time.Duration) string {
	t.Helper()
	tok, err := auth.GenerateToken(ws, user, provisionScopes, f.pk, life)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	return tok
}

func (f *skFixture) call(method, path, bearer, body string) *httptest.ResponseRecorder {
	var rdr io.Reader = http.NoBody
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

type mintedSessionKey struct {
	Key       string `json:"key"`
	ID        string `json:"id"`
	Prefix    string `json:"prefix"`
	ExpiresAt string `json:"expires_at"`
}

// ⚠ THE HEADLINE. A browser session mints a session key, and the key drives inference.
func TestSessionKeyMint_ABrowserSessionGetsAProxyCredential(t *testing.T) {
	pool := skHandlerPool(t)
	f := skRouter(t, pool, time.Hour)

	rec := f.call(http.MethodPost, "/v1/auth/session-keys", f.sessionJWT(t, "ws-mint", "user-a", 24*time.Hour), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	var out mintedSessionKey
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if !strings.HasPrefix(out.Key, sessionkey.KeyPrefix) {
		t.Fatalf("minted key %q lacks the session-key prefix", out.Key)
	}
	if out.ExpiresAt == "" {
		t.Fatal("the response does not say when the key dies — the caller cannot renew what it " +
			"cannot see expiring")
	}

	// It reaches the proxy gate. Same gate that refuses the session token that minted it.
	gate := auth.AuthMiddleware(auth.New(pool), f.mgr)(auth.RequireScope(auth.ScopeProxy)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })))
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+out.Key)
	grec := httptest.NewRecorder()
	gate.ServeHTTP(grec, req)
	if grec.Code != http.StatusOK {
		t.Fatalf("the minted session key was refused by the proxy gate: %d (%s)", grec.Code, grec.Body.String())
	}
}

// ⚠ THE REFUSAL THAT MATTERS MOST: A SESSION KEY CANNOT MINT A SESSION KEY.
//
// Without this, the credential renews itself forever and the TTL is decoration — the browser holds
// a permanent proxy credential wearing an hourly costume.
func TestSessionKeyMint_ASessionKeyCannotMintAnother(t *testing.T) {
	pool := skHandlerPool(t)
	f := skRouter(t, pool, time.Hour)

	raw, _, err := f.store.Mint(context.Background(), "ws-chain", "user-chain", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	rec := f.call(http.MethodPost, "/v1/auth/session-keys", raw, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a session key minted another with %d (%s), want 403. The TTL is meaningless if "+
			"the credential can renew itself", rec.Code, rec.Body.String())
	}
}

// ⚠ AND A WORKSPACE KEY CANNOT EITHER. A long-lived server-side key has no session to scope to;
// admitting it would produce a "session" key belonging to no session.
func TestSessionKeyMint_AWorkspaceKeyCannotMint(t *testing.T) {
	pool := skHandlerPool(t)
	f := skRouter(t, pool, time.Hour)

	raw, _, err := f.tenants.CreateAPIKey(context.Background(), "ws-wskey", "ci", []string{auth.ScopeProxy}, nil)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	rec := f.call(http.MethodPost, "/v1/auth/session-keys", raw, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a workspace API key minted a session key with %d (%s), want 403", rec.Code, rec.Body.String())
	}
}

func TestSessionKeyMint_NoCredentialIsUnauthorized(t *testing.T) {
	pool := skHandlerPool(t)
	f := skRouter(t, pool, time.Hour)
	if rec := f.call(http.MethodPost, "/v1/auth/session-keys", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated mint = %d (%s), want 401", rec.Code, rec.Body.String())
	}
}

// ⚠ THE CLAMP. A session key must never outlive the session that asked for it.
func TestSessionKeyMint_TTLIsClampedToTheCallersOwnRemainingLife(t *testing.T) {
	pool := skHandlerPool(t)
	// Configured ceiling is a generous 8h; the CALLER has only 4 minutes left.
	f := skRouter(t, pool, 8*time.Hour)

	rec := f.call(http.MethodPost, "/v1/auth/session-keys",
		f.sessionJWT(t, "ws-clamp", "user-clamp", 4*time.Minute), `{"ttl_seconds":86400}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	var out mintedSessionKey
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	exp, err := time.Parse(time.RFC3339, out.ExpiresAt)
	if err != nil {
		t.Fatalf("parse expires_at %q: %v", out.ExpiresAt, err)
	}
	if d := time.Until(exp); d > 5*time.Minute {
		t.Fatalf("the key lives %v, but the SESSION that minted it has ~4m left. A credential that "+
			"outlives its sign-in is a workspace key with extra steps (asked for 24h, configured "+
			"ceiling 8h)", d)
	}
}

// The configured ceiling binds when it is the smaller of the two.
func TestSessionKeyMint_TTLIsClampedToTheConfiguredCeiling(t *testing.T) {
	pool := skHandlerPool(t)
	f := skRouter(t, pool, 15*time.Minute)
	rec := f.call(http.MethodPost, "/v1/auth/session-keys",
		f.sessionJWT(t, "ws-ceil", "user-ceil", 30*24*time.Hour), `{"ttl_seconds":86400}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	var out mintedSessionKey
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	exp, err := time.Parse(time.RFC3339, out.ExpiresAt)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if d := time.Until(exp); d > 16*time.Minute {
		t.Fatalf("the key lives %v despite a 15m configured ceiling — the ceiling is decoration", d)
	}
}

// ⚠ SIGN-OUT. The route the BFF calls when the user signs out, asserted by the credential going
// dead rather than by the status code — a 200 is also what "deliberately did nothing" looks like.
func TestSessionKeyRevokeAll_IsWhatSignOutCallsAndItWorks(t *testing.T) {
	pool := skHandlerPool(t)
	f := skRouter(t, pool, time.Hour)
	jwt := f.sessionJWT(t, "ws-out", "user-out", 24*time.Hour)

	var keys []string
	for i := 0; i < 2; i++ {
		rec := f.call(http.MethodPost, "/v1/auth/session-keys", jwt, "")
		if rec.Code != http.StatusCreated {
			t.Fatalf("mint %d = %d (%s)", i, rec.Code, rec.Body.String())
		}
		var out mintedSessionKey
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		keys = append(keys, out.Key)
	}
	for _, k := range keys {
		if _, err := f.store.Validate(context.Background(), k); err != nil {
			t.Fatalf("a key must work BEFORE sign-out or this proves nothing: %v", err)
		}
	}

	rec := f.call(http.MethodDelete, "/v1/auth/session-keys", jwt, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("sign-out revoke = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	var out struct {
		Revoked int64 `json:"revoked"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Revoked != 2 {
		t.Fatalf("revoked = %d, want 2", out.Revoked)
	}
	for i, k := range keys {
		if _, err := f.store.Validate(context.Background(), k); err == nil {
			t.Fatalf("key %d still validates after sign-out — the session outlived the session", i)
		}
	}
}

// ⚠ REVOKE-BY-ID IS WORKSPACE-SCOPED, and the assertion is that the victim's key still WORKS —
// a 200/404 status assertion would pass against a route that revoked it and said so politely.
func TestSessionKeyRevokeByID_CannotReachAnotherWorkspacesKey(t *testing.T) {
	pool := skHandlerPool(t)
	f := skRouter(t, pool, time.Hour)

	rawVictim, victim, err := f.store.Mint(context.Background(), "ws-victim", "user-v", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	attacker := f.sessionJWT(t, "ws-attacker", "user-att", 24*time.Hour)
	f.call(http.MethodDelete, "/v1/auth/session-keys/"+victim.ID, attacker, "")

	if _, err := f.store.Validate(context.Background(), rawVictim); err != nil {
		t.Fatalf("another workspace revoked this key by id: %v", err)
	}
}

// ⚠ FLAG-OFF MUST MEAN "THE ROUTE DOES NOT EXIST", NOT "THE ROUTE REFUSES".
//
// A registered-but-refusing route is still an authz surface: it answers, it can be probed, and one
// future edit to its guard turns it on for everybody. The H5 flags take the not-registered posture
// and so does this. That is a property of main.go's WIRING, and the router is built inline inside
// run()'s full dependency graph with no exported seam a test can mount — the same limitation
// cmd/lens/admin_route_classification_test.go records — so this is asserted from the source that
// does the registering.
//
// ⚠ TWO-SIDED ON PURPOSE. It also asserts an UNCONDITIONAL neighbour (/v1/auth/token), so it cannot
// pass against a main.go where every route happens to be inside some conditional.
func TestSessionKeyRoutesAreRegisteredOnlyWhenConfigured(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(src)

	// The mount call must sit inside a guard on the constructed store.
	idx := strings.Index(text, "mountSessionKeyRoutes(authed,")
	if idx < 0 {
		t.Fatal("main.go never calls mountSessionKeyRoutes — the feature is dead code, and every " +
			"test in this file is then testing a router nothing serves")
	}
	// Look back a short way for the conditional that guards it.
	window := text[max(0, idx-400):idx]
	if !strings.Contains(window, "if sessionKeyStore != nil {") {
		t.Fatalf("mountSessionKeyRoutes is not guarded by `if sessionKeyStore != nil` — flag-off "+
			"would REGISTER routes that mint a proxy-capable credential.\nPreceding source:\n%s", window)
	}
	// And the store itself must only be constructed when the config says so.
	if !strings.Contains(text, "if cfg.SessionKeysEnabled {") {
		t.Fatal("main.go constructs the session-key store unconditionally — the config flag is decoration")
	}
	// The control: an unconditional neighbour on the same router.
	if !strings.Contains(text, `authed.Post("/v1/auth/token", newAuthTokenMintHandler(authManager))`) {
		t.Fatal("the unconditional control route /v1/auth/token is gone — re-anchor this census, " +
			"because without it the assertions above could pass against a main.go that registers nothing")
	}
}
