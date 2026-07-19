package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Scope enforcement, wired onto the proxy path. Until now all four scopes were
// DEAD — RequireScope had zero production callers, so a key created with
// scopes:["analytics"] (or none) could call the proxy. Wiring RequireScope onto
// the proxy path makes the proxy scope real, with two back-compat carve-outs:
//   1. an EMPTY scope set is grandfathered as all-scopes (every key minted
//      before enforcement has an empty set — silent access loss would be worse
//      than the leak we are closing);
//   2. a fast-path DB api_keys credential (auth.KeyStore) has no scope field at
//      all, so it is grandfathered too.
// Admin (the global key) is unchanged: IsAdmin short-circuits inside HasScope.

// withFastPathAPIKey injects an APIKey the way AuthMiddleware's DB fast path
// does (the apiKeyContextKey slot, no AuthContext) — the credential shape a
// key created via POST /v1/api/keys presents.
func withFastPathAPIKey(r *http.Request, k *APIKey) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, k))
}

func runProxyScopeGuard(t *testing.T, r *http.Request) int {
	t.Helper()
	h := RequireScope(ScopeProxy)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code
}

func proxyReq() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", nil)
}

// CONTROL: a workspace key that HAS the proxy scope passes the proxy path.
func TestRequireScope_ProxyScopePresent_Allows(t *testing.T) {
	r := withAuthContext(proxyReq(), &AuthContext{Scopes: []string{ScopeProxy}})
	if code := runProxyScopeGuard(t, r); code != http.StatusOK {
		t.Fatalf("proxy-scoped key on proxy path: want 200, got %d", code)
	}
}

// HEADLINE: a workspace key WITHOUT the proxy scope is refused on the proxy
// path — 403 (it authenticated fine; it is forbidden), never 401.
func TestRequireScope_ProxyScopeMissing_Forbids(t *testing.T) {
	r := withAuthContext(proxyReq(), &AuthContext{Scopes: []string{ScopeAnalytics}})
	if code := runProxyScopeGuard(t, r); code != http.StatusForbidden {
		t.Fatalf("analytics-only key on proxy path: want 403, got %d", code)
	}
}

// BACK-COMPAT 1: an empty scope set is grandfathered as all-scopes.
func TestRequireScope_EmptyScopesGrandfathered(t *testing.T) {
	r := withAuthContext(proxyReq(), &AuthContext{Scopes: nil})
	if code := runProxyScopeGuard(t, r); code != http.StatusOK {
		t.Fatalf("empty-scope key on proxy path: want 200 (grandfathered), got %d", code)
	}
}

// BACK-COMPAT 2: a fast-path api_keys credential has no scope field — it is
// grandfathered (it authenticated, and there is nothing to check against).
func TestRequireScope_FastPathAPIKeyGrandfathered(t *testing.T) {
	r := withFastPathAPIKey(proxyReq(), &APIKey{ID: "k1", WorkspaceID: "ws1", Active: true})
	if code := runProxyScopeGuard(t, r); code != http.StatusOK {
		t.Fatalf("fast-path api_keys credential on proxy path: want 200 (grandfathered), got %d", code)
	}
}

// CONTROL: admin (global key) is unchanged — passes via IsAdmin.
func TestRequireScope_AdminUnchanged(t *testing.T) {
	r := withAuthContext(proxyReq(), &AuthContext{
		IsAdmin: true,
		Scopes:  []string{ScopeProxy, ScopeAnalytics, ScopeAdmin, ScopeKeys},
	})
	if code := runProxyScopeGuard(t, r); code != http.StatusOK {
		t.Fatalf("admin key on proxy path: want 200, got %d", code)
	}
}

// CONTROL: a request with no credential at all (not behind AuthMiddleware) still
// 401s — the guard must not silently pass an unauthenticated request.
func TestRequireScope_NoCredential_Unauthorized(t *testing.T) {
	if code := runProxyScopeGuard(t, proxyReq()); code != http.StatusUnauthorized {
		t.Fatalf("no credential on proxy path: want 401, got %d", code)
	}
}
