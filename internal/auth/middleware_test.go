package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestManager returns a Manager with a fixed global key and JWT secret,
// no keyStore, no tenantStore — sufficient for middleware integration tests.
func newTestManager(globalKey, jwtSecret string) *Manager {
	return NewManager(globalKey, jwtSecret, newKeyStore(nil), nil)
}

// sentinel handler that records whether it was called and echoes the auth
// method from the stamped AuthContext.
func sentinelHandler(called *bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*called = true
		if actx := GetAuthContext(r.Context()); actx != nil {
			w.Header().Set("X-Auth-Method", actx.AuthMethod)
			if actx.IsAdmin {
				w.Header().Set("X-Is-Admin", "true")
			}
		}
		w.WriteHeader(http.StatusOK)
	}
}

// ── Global key ────────────────────────────────────────────────────────────────

// TestAuthMiddleware_GlobalKey_Reaches_Handler is the integration test Nicolai
// requested: exercises the real AuthMiddleware → handler path with the global
// admin key to prove it is no longer blocked at the gate.
func TestAuthMiddleware_GlobalKey_Reaches_Handler(t *testing.T) {
	const globalKey = "test-global-key-that-is-long-enough"
	m := newTestManager(globalKey, "")
	ks := newKeyStore(nil)

	var reached bool
	handler := AuthMiddleware(ks, m)(sentinelHandler(&reached))

	req := httptest.NewRequest(http.MethodGet, "/admin/something", nil)
	req.Header.Set("Authorization", "Bearer "+globalKey)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — global key was blocked at the gate", rr.Code)
	}
	if !reached {
		t.Fatal("handler was never called — global key did not pass middleware")
	}
	if rr.Header().Get("X-Is-Admin") != "true" {
		t.Error("expected IsAdmin=true in context for global key")
	}
	if rr.Header().Get("X-Auth-Method") != MethodGlobalKey {
		t.Errorf("expected auth method %q, got %q", MethodGlobalKey, rr.Header().Get("X-Auth-Method"))
	}
}

// TestAuthMiddleware_GlobalKey_XTalyvorKey checks the alternative header.
func TestAuthMiddleware_GlobalKey_XTalyvorKey(t *testing.T) {
	const globalKey = "test-global-key-that-is-long-enough"
	m := newTestManager(globalKey, "")
	ks := newKeyStore(nil)

	var reached bool
	handler := AuthMiddleware(ks, m)(sentinelHandler(&reached))

	req := httptest.NewRequest(http.MethodGet, "/admin/something", nil)
	req.Header.Set("X-Talyvor-Key", globalKey)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 via X-Talyvor-Key, got %d", rr.Code)
	}
	if !reached {
		t.Fatal("handler was never called")
	}
}

// ── JWT ───────────────────────────────────────────────────────────────────────

// TestAuthMiddleware_JWT_Reaches_Handler proves that a valid JWT is no longer
// blocked — previously Validate() hashed the raw token string, looked it up
// in api_keys, got no rows, and returned 401 before the handler ran.
func TestAuthMiddleware_JWT_Reaches_Handler(t *testing.T) {
	secret := strings.Repeat("s", 32)
	m := newTestManager("", secret)
	ks := newKeyStore(nil)

	tok, err := GenerateToken("ws_jwt", "user_1", []string{ScopeProxy}, secret, time.Hour)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	var reached bool
	handler := AuthMiddleware(ks, m)(sentinelHandler(&reached))

	req := httptest.NewRequest(http.MethodGet, "/v1/auth/refresh", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for valid JWT, got %d — JWT was blocked at the gate", rr.Code)
	}
	if !reached {
		t.Fatal("handler was never called for valid JWT")
	}
	if rr.Header().Get("X-Auth-Method") != MethodJWT {
		t.Errorf("expected auth method %q, got %q", MethodJWT, rr.Header().Get("X-Auth-Method"))
	}
}

// TestAuthMiddleware_JWT_Expired_Blocked proves an expired JWT is correctly
// rejected even after the Manager fallback.
func TestAuthMiddleware_JWT_Expired_Blocked(t *testing.T) {
	secret := strings.Repeat("s", 32)
	m := newTestManager("", secret)
	ks := newKeyStore(nil)

	tok, err := GenerateToken("ws_jwt", "user_1", []string{ScopeProxy}, secret, -time.Hour)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	var reached bool
	handler := AuthMiddleware(ks, m)(sentinelHandler(&reached))

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for expired JWT, got %d", rr.Code)
	}
	if reached {
		t.Fatal("handler must not be called for expired JWT")
	}
}

// ── Missing / invalid credentials ────────────────────────────────────────────

// TestAuthMiddleware_NoCredential_Returns401 verifies the baseline: no header
// → 401 immediately, handler never called.
func TestAuthMiddleware_NoCredential_Returns401(t *testing.T) {
	m := newTestManager("global", strings.Repeat("s", 32))
	ks := newKeyStore(nil)

	var reached bool
	handler := AuthMiddleware(ks, m)(sentinelHandler(&reached))

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
	if reached {
		t.Fatal("handler must not be called without credentials")
	}
}

// TestAuthMiddleware_WrongGlobalKey_Returns401 verifies a wrong key is rejected
// even when a valid global key is configured.
func TestAuthMiddleware_WrongGlobalKey_Returns401(t *testing.T) {
	m := newTestManager("correct-key", "")
	ks := newKeyStore(nil)

	var reached bool
	handler := AuthMiddleware(ks, m)(sentinelHandler(&reached))

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong key, got %d", rr.Code)
	}
	if reached {
		t.Fatal("handler must not be called for wrong key")
	}
}

// ── AuthContext stamping ──────────────────────────────────────────────────────

// TestAuthMiddleware_GlobalKey_StampsAuthContext verifies that the Manager
// fallback path stamps both apiKeyContextKey and authContextCtxKey so
// downstream handlers can use either GetAPIKey or GetAuthContext.
func TestAuthMiddleware_GlobalKey_StampsAuthContext(t *testing.T) {
	const globalKey = "test-global-key-that-is-long-enough"
	m := newTestManager(globalKey, "")
	ks := newKeyStore(nil)

	var gotAPIKey *APIKey
	var gotAuthCtx *AuthContext
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = GetAPIKey(r.Context())
		gotAuthCtx = GetAuthContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	handler := AuthMiddleware(ks, m)(inner)

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.Header.Set("Authorization", "Bearer "+globalKey)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if gotAPIKey == nil {
		t.Error("GetAPIKey returned nil after global key auth — rate-limiter would panic")
	}
	if gotAuthCtx == nil {
		t.Error("GetAuthContext returned nil after global key auth — IsAdmin check would fail")
	}
	if gotAuthCtx != nil && !gotAuthCtx.IsAdmin {
		t.Error("expected IsAdmin=true for global key")
	}
}
