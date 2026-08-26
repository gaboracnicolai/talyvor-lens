package main

// session_credential_reach_test.go — WHAT THE BROWSER'S SESSION CREDENTIAL CAN ACTUALLY REACH.
//
// ⚠ WHY THIS FILE EXISTS. W4.6.1's step-3 note records, correctly, that the BFF's provisioned
// session token is refused by /v1/proxy/* with `forbidden: missing scope proxy`, and concludes that
// giving that token the proxy scope would be "an authz decision on a money path: every signed-in
// browser session would then be able to drive inference against its workspace's balance."
//
// ⚠ THE PREMISE OF THAT CONCLUSION IS NOT TRUE TODAY, AND THIS FILE MEASURES IT RATHER THAN
// ARGUING IT. A signed-in browser session ALREADY reaches inference — not through /v1/proxy/* with
// the session token, but by asking the SAME session token to mint a workspace API key carrying
// scopes:["proxy"] and then using that. `POST /v1/workspaces/{wsID}/api-keys` carries NO scope
// gate; the only middleware in front of it is workspaceIsolationMiddleware, which asks "do you own
// this workspace", never "may you grant this scope".
//
// ⚠ AND IT IS NOT A THEORETICAL PATH — IT IS THE SHIPPED KEYS SCREEN. In talyvor-suite, the BFF's
// keys handler (apps/bff/keys.go, handleMintKey — cited WITHOUT the path#symbol form on purpose:
// this repo's pointer audit resolves that form against THIS tree, and a suite path never can)
// decodes {name, scopes, expires_at}, re-encodes them, and POSTs
// them to exactly that Lens route with `Authorization: Bearer <the SESSION's workspace token>`.
// The client's `scopes` are forwarded verbatim. That handler's own comment already names the
// consequence — "a console-minted key that satisfies the proxy gate which spends the workspace's
// credit".
//
// ⚠ SO THE COMPARISON THAT MATTERS IS NOT "proxy scope vs no proxy scope". It is:
//
//     the session token with proxy   →  scoped to the session, dies with the JWT's exp
//     what the session can mint today →  a workspace-WIDE key, no TTL unless the caller asks for
//                                        one, unaffected by sign-out, returned to the browser in
//                                        plaintext, and revocable only by explicit user action
//
// The credential reachable today is the STRICTLY WIDER of the two. That is the measurement W4.6.1
// step 4 (session-scoped keys) needs on the table before anyone decides step 3, and it is why this
// file asserts the reach instead of describing it.
//
// ⚠ NOTHING HERE CHANGES BEHAVIOUR. Every assertion below is a statement about code as it stands at
// this commit. Two of them are guards that must never go red (the session token is refused proxy;
// a proxy-scoped workspace key is accepted). One of them — TestMeasured_SessionTokenCanMintItself
// AProxyScopedWorkspaceKey — records a gap, and its failure message says so: if it goes red because
// the mint route learned to check the caller's own scopes, that is the FIX landing and this test
// should be deleted together with the queue note that carries it.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/tenant"
)

// sessReachSchema gives this test its own schema and its own tables.
//
// ⚠ NOT THE MIGRATED public SCHEMA, for the reason internal/tenant's revocation test already
// records: other packages in the same `-p 1` run DROP SCHEMA public outright, so a test that
// assumes migrations are still applied passes alone and fails on package order.
const sessReachSchema = "lens_it_sessreach"

func sessReachPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG session-credential reach test")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = sessReachSchema
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(context.Background(), `
		DROP SCHEMA IF EXISTS `+sessReachSchema+` CASCADE;
		CREATE SCHEMA `+sessReachSchema+`;
		CREATE TABLE `+sessReachSchema+`.workspace_api_keys (
			id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			workspace_id TEXT NOT NULL,
			key_hash     TEXT NOT NULL,
			key_prefix   TEXT NOT NULL,
			name         TEXT NOT NULL DEFAULT '',
			scopes       TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[],
			last_used_at TIMESTAMPTZ,
			expires_at   TIMESTAMPTZ,
			created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE INDEX ON `+sessReachSchema+`.workspace_api_keys (key_prefix);
		-- Empty api_keys so AuthMiddleware's fast path cleanly MISSES for tlv_ws_ keys and
		-- falls through to Manager.Authenticate, which is the only place the workspace-key
		-- branch runs. Without this table the fast path errors instead of missing.
		CREATE TABLE `+sessReachSchema+`.api_keys (
			id           TEXT PRIMARY KEY,
			key_hash     TEXT UNIQUE NOT NULL,
			workspace_id TEXT NOT NULL DEFAULT 'default',
			team         TEXT NOT NULL DEFAULT '',
			name         TEXT NOT NULL DEFAULT '',
			active       BOOLEAN NOT NULL DEFAULT true,
			created_at   TIMESTAMPTZ DEFAULT NOW(),
			last_used_at TIMESTAMPTZ,
			expires_at   TIMESTAMPTZ
		);
	`); err != nil {
		t.Fatalf("build fixture schema: %v", err)
	}
	return pool
}

// sessReachStack builds the REAL credential stack: the real KeyStore, the real tenant.Store, the
// real Manager. No seam is faked — the only thing this function assembles is the wiring, and the
// wiring it assembles is the wiring cmd/lens/main.go assembles at line ~2116.
func sessReachStack(t *testing.T, pool *pgxpool.Pool) (*auth.Manager, *tenant.Store, *ecdsa.PrivateKey) {
	t.Helper()
	pk, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	store := tenant.NewStore(pool)
	keyStore := auth.New(pool)
	return auth.NewManager("sessreach-global-admin-key", pk, keyStore, store), store, pk
}

// proxyGateHandler is the /v1/proxy/* middleware chain EXACTLY as main.go composes it:
//
//	authed.Use(auth.AuthMiddleware(keyStore, authManager))
//	proxyScope := auth.RequireScope(auth.ScopeProxy)
//	authed.With(proxyScope).Post("/v1/proxy/openai/*", ...)
//
// The terminal handler stands in for p.HandleOpenAI — reaching it at all IS the property under
// test, and a real provider call would prove nothing extra about authorization.
func proxyGateHandler(m *auth.Manager, pool *pgxpool.Pool) http.Handler {
	r := chi.NewRouter()
	r.Use(auth.AuthMiddleware(auth.New(pool), m))
	r.With(auth.RequireScope(auth.ScopeProxy)).Post("/v1/proxy/openai/*", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"reached":"inference"}`))
	})
	return r
}

// mintRouteHandler is the /v1/workspaces/{wsID}/api-keys chain EXACTLY as main.go composes it:
// AuthMiddleware, then workspaceIsolationMiddleware, then the handler. ⚠ THE ABSENCE OF A SCOPE
// MIDDLEWARE HERE IS NOT AN OMISSION IN THE FIXTURE — it mirrors main.go, where that registration
// is the bare `authed.Post(...)` with no `.With(...)`. TestMintRouteRegistrationCarriesNoScopeGate
// below asserts that against main.go's own source so this fixture cannot drift into flattering
// itself.
func mintRouteHandler(m *auth.Manager, pool *pgxpool.Pool, store *tenant.Store) http.Handler {
	r := chi.NewRouter()
	// ⚠ r.Group, NOT chi.NewRouter().Use — AND THE DIFFERENCE IS LOAD-BEARING, NOT STYLISTIC.
	//
	// A middleware registered with Use on a top-level Mux runs BEFORE the route pattern is
	// matched, so chi.URLParam(r, "wsID") returns "" inside it — and workspaceIsolationMiddleware
	// short-circuits on exactly that (`if wsID := chi.URLParam(...); wsID != ""`). The first
	// version of this fixture did that, and control C5 caught it: denying every caller in
	// workspaceAuthorized left the escalation test GREEN, because the isolation check was never
	// reached at all. The fixture was more permissive than the product it claimed to model.
	//
	// r.Group returns an INLINE mux, whose middlewares are chained around the ENDPOINT handler and
	// therefore run after routing with the params populated. That is what main.go does
	// (`r.Group(func(authed chi.Router) { authed.Use(auth.AuthMiddleware(...)); authed.Use(
	// workspaceIsolationMiddleware); ... })`), so it is what this must do.
	r.Group(func(authed chi.Router) {
		authed.Use(auth.AuthMiddleware(auth.New(pool), m))
		authed.Use(workspaceIsolationMiddleware)
		authed.Post("/v1/workspaces/{wsID}/api-keys", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			raw, _, err := store.CreateAPIKey(req.Context(), wsID, "browser-minted", []string{auth.ScopeProxy}, nil)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"key":"` + raw + `"}`))
		})
	})
	return r
}

// ─── the guard that must never go red ──────────────────────────────────────

// TestProvisionedSessionToken_IsRefusedByTheProxyGate pins the refusal W4.6.1 step 3 measured.
//
// ⚠ THIS IS THE GUARD AGAINST THE DECISION THE QUEUE FORBIDS: adding auth.ScopeProxy to
// provisionScopes turns this red. It reads the REAL provisionScopes var, so it cannot drift from
// what the provision route actually mints.
func TestProvisionedSessionToken_IsRefusedByTheProxyGate(t *testing.T) {
	pool := sessReachPool(t)
	m, _, pk := sessReachStack(t, pool)

	const ws = "u-sessreach-browser"
	tok, err := auth.GenerateToken(ws, ws, provisionScopes, pk, time.Hour)
	if err != nil {
		t.Fatalf("mint session token: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	proxyGateHandler(m, pool).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("the provisioned session token reached the proxy gate with %d (%s), want 403.\n"+
			"provisionScopes is currently %v. If proxy was just added to it, that is the authz "+
			"decision W4.6.1 records as Nicolai's and this queue forbids a session making — revert "+
			"it and take the decision, or build W4.6.1 step 4 (session-scoped keys) instead.",
			rec.Code, rec.Body.String(), provisionScopes)
	}
}

// TestProxyScopedWorkspaceKey_IsAcceptedByTheProxyGate is the other end of the same gate, and it
// is a control: without it, the test above would also pass against a proxy gate that refuses
// EVERYTHING, which would prove nothing about scopes at all.
func TestProxyScopedWorkspaceKey_IsAcceptedByTheProxyGate(t *testing.T) {
	pool := sessReachPool(t)
	m, store, _ := sessReachStack(t, pool)

	raw, _, err := store.CreateAPIKey(context.Background(), "u-sessreach-control", "control", []string{auth.ScopeProxy}, nil)
	if err != nil {
		t.Fatalf("mint workspace key: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	proxyGateHandler(m, pool).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("a proxy-scoped workspace key was refused with %d (%s), want 200 — the proxy gate "+
			"now refuses the credential it exists to admit, and every customer key is broken",
			rec.Code, rec.Body.String())
	}
}

// ─── the measurement: the gap ──────────────────────────────────────────────

// TestMeasured_SessionTokenCanMintItselfAProxyScopedWorkspaceKey is the finding.
//
// It drives the session token — the very credential the test above proves cannot reach inference —
// through the mint route's real middleware chain, and then presents the key it gets back to the
// proxy gate. Both halves use product code.
//
// ⚠ IF THIS GOES RED, READ THE STATUS CODE BEFORE ASSUMING A BREAKAGE:
//   - 403 from the MINT call means the mint route learned to check the caller's own scopes. That is
//     the fix. Delete this test and the W4.6.1 note that carries it.
//   - 403 from the PROXY call means something narrowed workspace keys instead, which would break
//     every customer key — see the control above.
func TestMeasured_SessionTokenCanMintItselfAProxyScopedWorkspaceKey(t *testing.T) {
	pool := sessReachPool(t)
	m, store, pk := sessReachStack(t, pool)

	const ws = "u-sessreach-escalate"
	tok, err := auth.GenerateToken(ws, ws, provisionScopes, pk, time.Hour)
	if err != nil {
		t.Fatalf("mint session token: %v", err)
	}

	// 1. The session token asks for a workspace key carrying the one scope it does NOT hold.
	req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+ws+"/api-keys", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	mintRouteHandler(m, pool, store).ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("MINT refused with %d (%s). If this is 403 the mint route now checks the caller's "+
			"own scopes — that is the fix landing; delete this test. want 201 at this commit",
			rec.Code, rec.Body.String())
	}
	var minted struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil {
		t.Fatalf("decode mint response: %v (%s)", err, rec.Body.String())
	}
	if minted.Key == "" {
		t.Fatalf("mint returned 201 with no key: %s", rec.Body.String())
	}

	// 2. The key it just minted drives inference. Same gate, same process, same request shape as
	//    the 403 above — the ONLY thing that changed is which credential is in the header.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", http.NoBody)
	req2.Header.Set("Authorization", "Bearer "+minted.Key)
	rec2 := httptest.NewRecorder()
	proxyGateHandler(m, pool).ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("the browser-minted key was refused by the proxy gate with %d (%s) — see the "+
			"control test; want 200 at this commit", rec2.Code, rec2.Body.String())
	}

	t.Logf("MEASURED: a session token carrying %v cannot reach /v1/proxy/* (403), and CAN mint "+
		"itself a workspace-wide key carrying [proxy] which reaches it (200). The refusal bounds "+
		"one credential shape, not the capability.", provisionScopes)
}

// ─── the link between the fixture and the product ──────────────────────────

// TestMintRouteRegistrationCarriesNoScopeGate reads main.go's OWN registration text.
//
// ⚠ WHY FROM SOURCE. The fixtures above compose the middleware chain by hand, because the router is
// built inline inside run()'s full dependency graph and there is no exported seam that returns a
// mounted router — cmd/lens/admin_route_classification_test.go records that same limitation and
// takes the same way out. A hand-composed chain proves nothing if it is more generous than the
// product, so this test asserts the two registrations the fixtures model, from the file that
// actually registers them:
//
//   - /v1/proxy/openai/*                  MUST carry a scope gate  (`.With(proxyScope)`)
//   - /v1/workspaces/{wsID}/api-keys POST MUST NOT carry one       (bare `authed.Post`)
//
// ⚠ IT IS DELIBERATELY TWO-SIDED. A test that only asserted the absence would also pass against a
// main.go where NOTHING is gated — the shape that lets a gate quietly disappear. Asserting the
// gated route in the same pass means this file cannot go green over a router that gates nothing.
func TestMintRouteRegistrationCarriesNoScopeGate(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	var gated, mint string
	for _, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.Contains(trimmed, `Post("/v1/proxy/openai/*"`):
			gated = trimmed
		case strings.Contains(trimmed, `Post("/v1/workspaces/{wsID}/api-keys"`):
			mint = trimmed
		}
	}
	if gated == "" || mint == "" {
		t.Fatalf("could not find both registrations in main.go (proxy=%q mint=%q) — a route was "+
			"renamed or moved and this census has gone blind; re-anchor it", gated, mint)
	}
	if !strings.Contains(gated, "With(proxyScope)") {
		t.Fatalf("/v1/proxy/openai/* is registered WITHOUT a scope gate:\n  %s\n"+
			"the whole proxy-scope boundary is gone", gated)
	}
	if strings.Contains(mint, ".With(") {
		t.Fatalf("/v1/workspaces/{wsID}/api-keys now carries middleware:\n  %s\n"+
			"if that is a scope gate, the escalation measured in this file is CLOSED — delete "+
			"TestMeasured_SessionTokenCanMintItselfAProxyScopedWorkspaceKey and the W4.6.1 note "+
			"that carries it", mint)
	}
}

// TestRequireScopeHasExactlyOneCallSite is the second half of the same measurement, and it is the
// one that explains WHY the mint route has no gate: nothing else in this binary has one either.
//
// ⚠ SIX SCOPE CONSTANTS ARE DECLARED IN internal/auth/manager.go AND RequireScope IS CALLED WITH
// EXACTLY ONE OF THEM. `analytics` and `keys` — the two the provision route mints — gate nothing at
// all. Their only operative effect is to make the scope set NON-EMPTY, which switches OFF
// RequireScope's empty-set grandfather and thereby DENIES proxy. They subtract a capability; they
// grant none. (`admin` is enforced by requireAdmin reading actx.IsAdmin, and `mint` /
// `operator_read` by their own by-name checks — none of those three go through RequireScope, which
// is why this test counts call sites rather than asserting each scope is unenforced.)
func TestRequireScopeHasExactlyOneCallSite(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	var sites []string
	for _, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.Contains(trimmed, "auth.RequireScope(") {
			sites = append(sites, trimmed)
		}
	}
	if len(sites) != 1 || !strings.Contains(sites[0], "auth.ScopeProxy") {
		t.Fatalf("auth.RequireScope call sites in main.go = %d, want exactly 1 carrying "+
			"auth.ScopeProxy.\nFound: %q\nIf a scope gate was ADDED, that is a real narrowing — "+
			"update this test and say which credential it refuses. If one was REMOVED, a scope "+
			"boundary just disappeared silently.", len(sites), sites)
	}
}
