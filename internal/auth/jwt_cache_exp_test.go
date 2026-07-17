package auth

// jwt_cache_exp_test.go — sharp-edge #2: the JWT validation cache must never
// outlive the credential it caches. validateJWTCached previously cached claims
// until insert_time + jwtCacheTTL (5m) WITHOUT re-checking the token's own
// ExpiresAt — so a token expiring 1s after first use stayed accepted for ~5
// more minutes. The cache entry's lifetime is now min(token exp, cache TTL).

import (
	"testing"
	"time"
)

// TestJWTCache_HonorsTokenExp — behavioral proof: seed the cache with a
// short-lived token, wait past its exp, and require rejection. Under the old
// code the second Authenticate served the cached claims (only ~300ms into the
// 5-minute cache window) and ACCEPTED the expired token.
func TestJWTCache_HonorsTokenExp(t *testing.T) {
	key := testKey(t)
	mgr := NewManager("", key, nil, nil)
	// NOTE: jwt.NewNumericDate truncates exp to whole seconds, so a
	// sub-second TTL can floor into the past and mint an already-expired
	// token. 2s is the shortest reliable TTL — still 150x below the 5m
	// cache TTL, so the bound is genuinely exercised.
	tok, err := GenerateToken("ws_exp", "u", []string{ScopeProxy}, key, 2*time.Second)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	// First use: valid, seeds the cache.
	if _, err := mgr.Authenticate(newReq(tok)); err != nil {
		t.Fatalf("fresh token must authenticate: %v", err)
	}
	mgr.mu.RLock()
	e, cached := mgr.jwtCache[tok]
	mgr.mu.RUnlock()
	if !cached {
		t.Fatal("expected JWT cache to be populated")
	}
	// White-box: the entry must be bounded by the token's exp, not the 5m TTL.
	if e.expires.After(time.Now().Add(3 * time.Second)) {
		t.Fatalf("cache entry expires %v — outlives the token's own exp (~2s); the entry must be bounded by min(exp, cacheTTL)", e.expires)
	}

	// Past the token's exp: the cache must not resurrect it.
	time.Sleep(2100 * time.Millisecond)
	if _, err := mgr.Authenticate(newReq(tok)); err == nil {
		t.Fatal("token past its exp was accepted from the validation cache — cache lifetime must be bounded by the credential lifetime")
	}
}

// TestJWTCache_LongLivedTokenStillCached — the optimisation is preserved: a
// token whose exp is far beyond the cache TTL still gets the full cache TTL.
func TestJWTCache_LongLivedTokenStillCached(t *testing.T) {
	key := testKey(t)
	mgr := NewManager("", key, nil, nil)
	tok, err := GenerateToken("ws_long", "u", []string{ScopeProxy}, key, time.Hour)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if _, err := mgr.Authenticate(newReq(tok)); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	mgr.mu.RLock()
	e, cached := mgr.jwtCache[tok]
	mgr.mu.RUnlock()
	if !cached {
		t.Fatal("expected JWT cache to be populated")
	}
	// The entry should get (about) the full cache TTL — not be truncated.
	if e.expires.Before(time.Now().Add(jwtCacheTTL - 30*time.Second)) {
		t.Fatalf("cache entry expires %v — a long-lived token should keep the full %v cache TTL", e.expires, jwtCacheTTL)
	}
	if _, err := mgr.Authenticate(newReq(tok)); err != nil {
		t.Fatalf("cached long-lived token must still authenticate: %v", err)
	}
}
