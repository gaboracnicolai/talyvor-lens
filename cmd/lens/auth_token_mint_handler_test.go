package main

import (
	"crypto/ecdsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/auth"
)

// auth_token_mint_handler_test.go — Property 1 (admin-scoped minting) proof.
//
// The per-workspace-credential fix (step 1) needs a way to OBTAIN a per-tenant JWT from the
// one admin key. /v1/auth/token is that capability: ADMIN-ONLY, mints a JWT bound to an
// ARBITRARY tenant workspace. This proves the two halves that make it safe:
//   - only the admin key may mint for an arbitrary workspace (a non-admin credential is 403);
//   - the minted token round-trips: it authenticates back as EXACTLY that workspace (so it can
//     then key the rate-limit bucket and the spend attribution per-tenant — properties 2 & 3).

const mintTestGlobalKey = "test-global-admin-key-long-enough-xx"

// mintTestManager builds a real auth.Manager (global admin key + ES256 signing key, no DB) —
// the same shape main wires, sufficient to exercise Authenticate + GenerateToken end to end.
func mintTestManager(t *testing.T) (*auth.Manager, *ecdsa.PrivateKey) {
	t.Helper()
	jwtKey, err := auth.GenerateECKey()
	if err != nil {
		t.Fatalf("GenerateECKey: %v", err)
	}
	return auth.NewManager(mintTestGlobalKey, jwtKey, auth.New(nil), nil), jwtKey
}

// mintToken POSTs to the handler with the given credential + body and returns the recorder.
func mintToken(h http.HandlerFunc, bearer, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/token", strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// TestAuthTokenMint_AdminMintsArbitraryWorkspace_RoundTrips is the core capability: the admin
// key mints a JWT for a tenant workspace it does not itself belong to, and that token
// authenticates back as that exact workspace.
func TestAuthTokenMint_AdminMintsArbitraryWorkspace_RoundTrips(t *testing.T) {
	am, _ := mintTestManager(t)
	h := newAuthTokenMintHandler(am)

	rec := mintToken(h, mintTestGlobalKey, `{"workspace_id":"wsA","user_id":"u1","scopes":["proxy"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin mint: status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode mint response: %v (body: %s)", err, rec.Body.String())
	}
	if out.Token == "" {
		t.Fatal("admin mint returned an empty token")
	}

	// ROUND TRIP — the minted token must authenticate back as wsA (a NON-admin identity),
	// so it can key the per-tenant rate-limit bucket and spend attribution downstream.
	rt := httptest.NewRequest(http.MethodGet, "/v1/proxy/anthropic/x", nil)
	rt.Header.Set("Authorization", "Bearer "+out.Token)
	actx, err := am.Authenticate(rt)
	if err != nil {
		t.Fatalf("minted token failed to authenticate: %v", err)
	}
	if actx.WorkspaceID != "wsA" {
		t.Errorf("minted token authenticates as workspace %q, want wsA — the per-tenant credential is broken", actx.WorkspaceID)
	}
	if actx.IsAdmin {
		t.Error("minted per-workspace token must NOT be admin (it is a tenant credential, not the admin key)")
	}
	if actx.AuthMethod != auth.MethodJWT {
		t.Errorf("minted token auth method = %q, want %q", actx.AuthMethod, auth.MethodJWT)
	}
}

// TestAuthTokenMint_NonAdminRefused proves ONLY the admin key may mint for an arbitrary
// workspace: a valid non-admin credential (a workspace JWT) is refused 403 and gets no token.
// Without this gate, any tenant could mint a credential for any other tenant.
func TestAuthTokenMint_NonAdminRefused(t *testing.T) {
	am, jwtKey := mintTestManager(t)
	h := newAuthTokenMintHandler(am)

	// A legitimate non-admin caller: a workspace-scoped JWT for wsB.
	wsToken, err := auth.GenerateToken("wsB", "u", []string{auth.ScopeProxy}, jwtKey, time.Hour)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	// It tries to mint a token for wsC (a workspace it is not).
	rec := mintToken(h, wsToken, `{"workspace_id":"wsC"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin mint: status = %d, want 403 (a tenant must not mint for an arbitrary workspace)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "token") && strings.Contains(rec.Body.String(), "eyJ") {
		t.Error("non-admin refusal must not leak a minted token")
	}
}

// TestAuthTokenMint_NoCredentialRefused: an unauthenticated caller cannot mint. Fail-closed.
func TestAuthTokenMint_NoCredentialRefused(t *testing.T) {
	am, _ := mintTestManager(t)
	h := newAuthTokenMintHandler(am)
	if rec := mintToken(h, "", `{"workspace_id":"wsA"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("no-credential mint: status = %d, want 403", rec.Code)
	}
}

// TestAuthTokenMint_EmptyWorkspaceRejected: a mint must name a workspace — an empty
// workspace_id would produce a token that resolves to "" (the same collapse the whole fix
// removes), so it is a 400.
func TestAuthTokenMint_EmptyWorkspaceRejected(t *testing.T) {
	am, _ := mintTestManager(t)
	h := newAuthTokenMintHandler(am)
	if rec := mintToken(h, mintTestGlobalKey, `{"workspace_id":""}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty workspace mint: status = %d, want 400", rec.Code)
	}
}
