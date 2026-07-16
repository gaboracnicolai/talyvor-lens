package ratelimit_test

// perworkspace_isolation_integration_test.go — Property 2 (rate-limit isolation).
//
// Drives the REAL request chain — auth.AuthMiddleware → ratelimit.RateLimitMiddleware → 200 —
// so the bucket key is built exactly as production builds it: wsID from the credential
// (AuthMiddleware overwrites X-Talyvor-Workspace), keyID from the validated APIKey on context.
//
// The contrast IS the proof:
//   - GLOBAL KEY (the defect): every caller resolves to workspace "" (manager.go global branch),
//     so wsA-labelled and wsB-labelled traffic share one bucket lens:rl::global:* — exhausting
//     it with one tenant 429s the other. A cross-tenant outage.
//   - PER-WORKSPACE JWT (the fix): a wsA JWT and a wsB JWT resolve to their own trusted
//     workspaces, so they key DISTINCT buckets — exhausting wsA's returns 429 to wsA while wsB
//     is still served.

import (
	"crypto/ecdsa"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/ratelimit"
)

const isoGlobalKey = "test-global-admin-key-long-enough-xx"

// isoChain builds the real auth→ratelimit chain over a miniredis-backed limiter whose only rule
// is a tiny per-minute cap, so exhaustion is deterministic within a test (the minute window is
// stable across the few milliseconds a test runs).
func isoChain(t *testing.T, perMinute int) (http.Handler, *ecdsa.PrivateKey) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })

	jwtKey, err := auth.GenerateECKey()
	if err != nil {
		t.Fatalf("GenerateECKey: %v", err)
	}
	am := auth.NewManager(isoGlobalKey, jwtKey, auth.New(nil), nil)
	limiter := ratelimit.New(rc, []ratelimit.RateRule{{RequestsPerMinute: perMinute}})

	sentinel := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	chain := auth.AuthMiddleware(auth.New(nil), am)(ratelimit.RateLimitMiddleware(limiter)(sentinel))
	return chain, jwtKey
}

// isoMintWS mints a workspace-scoped JWT for ws via the same signing key the chain verifies with.
func isoMintWS(t *testing.T, key *ecdsa.PrivateKey, ws string) string {
	t.Helper()
	tok, err := auth.GenerateToken(ws, "u", []string{auth.ScopeProxy}, key, time.Hour)
	if err != nil {
		t.Fatalf("GenerateToken(%s): %v", ws, err)
	}
	return tok
}

// isoSend fires one request carrying the bearer credential and an optional client-supplied
// X-Talyvor-Workspace label (which AuthMiddleware must overwrite), returning the status code.
func isoSend(chain http.Handler, bearer, wsLabel string) int {
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/x", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	if wsLabel != "" {
		req.Header.Set("X-Talyvor-Workspace", wsLabel) // the forgeable label — must NOT decide the bucket
	}
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)
	return rec.Code
}

// TestRateLimit_PerWorkspaceJWT_Isolated is the FIX: two per-workspace JWTs get separate buckets.
func TestRateLimit_PerWorkspaceJWT_Isolated(t *testing.T) {
	const perMinute = 3
	chain, key := isoChain(t, perMinute)
	wsAtok := isoMintWS(t, key, "wsA")
	wsBtok := isoMintWS(t, key, "wsB")

	// Exhaust wsA's bucket: perMinute allowed, then 429.
	for i := 1; i <= perMinute; i++ {
		if code := isoSend(chain, wsAtok, ""); code != http.StatusOK {
			t.Fatalf("wsA request %d: status %d, want 200 (under its own limit)", i, code)
		}
	}
	if code := isoSend(chain, wsAtok, ""); code != http.StatusTooManyRequests {
		t.Fatalf("wsA request %d: status %d, want 429 (its bucket is exhausted)", perMinute+1, code)
	}

	// wsB is UNAFFECTED — a distinct trusted workspace keys a distinct bucket.
	if code := isoSend(chain, wsBtok, ""); code != http.StatusOK {
		t.Fatalf("wsB request after wsA exhausted: status %d, want 200 — per-workspace JWTs must NOT share a bucket", code)
	}
}

// TestRateLimit_GlobalKey_CollapsesCrossTenant is the DEFECT the fix removes: the shared global
// key resolves every tenant to workspace "", so a wsA-labelled flood 429s wsB-labelled traffic.
// (The X-Talyvor-Workspace labels are overwritten to "" by AuthMiddleware — trying to use them
// to separate tenants is exactly the #146 spoof the overwrite prevents.)
func TestRateLimit_GlobalKey_CollapsesCrossTenant(t *testing.T) {
	const perMinute = 3
	chain, _ := isoChain(t, perMinute)

	// Tenant A floods using the global key, labelling itself wsA.
	for i := 1; i <= perMinute; i++ {
		if code := isoSend(chain, isoGlobalKey, "wsA"); code != http.StatusOK {
			t.Fatalf("global-key wsA request %d: status %d, want 200", i, code)
		}
	}
	// Tenant B, a DIFFERENT tenant labelling itself wsB, is now 429'd — it never sent a request
	// of its own, yet A exhausted the single shared bucket lens:rl::global:*. Cross-tenant outage.
	if code := isoSend(chain, isoGlobalKey, "wsB"); code != http.StatusTooManyRequests {
		t.Fatalf("global-key wsB (a different tenant): status %d, want 429 — this collapse is the defect "+
			"per-workspace JWTs remove (a workspace label cannot separate global-key tenants)", code)
	}
}
