package auth_test

// forgery_rejection_test.go — Property 4 (forgery rejection).
//
// The per-workspace JWT is only a safe metering identity if its workspace claim cannot be forged.
// A JWT is signed (ES256) over header.payload; rewriting the workspace_id without the private key
// invalidates the signature. These prove a forged workspace claim is rejected at the gate and can
// never be metered/attributed under the forged workspace.

import (
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/auth"
)

const forgeGlobalKey = "test-global-admin-key-long-enough-xx"

func forgeSetup(t *testing.T) (*auth.Manager, *ecdsa.PrivateKey) {
	t.Helper()
	k, err := auth.GenerateECKey()
	if err != nil {
		t.Fatalf("GenerateECKey: %v", err)
	}
	return auth.NewManager(forgeGlobalKey, k, auth.New(nil), nil), k
}

// tamperWorkspaceClaim rewrites workspace_id in a signed JWT's payload WITHOUT re-signing — a
// forged workspace claim. The original signature no longer matches the modified header.payload.
func tamperWorkspaceClaim(t *testing.T, token, newWS string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %d segments", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if claims["workspace_id"] == newWS {
		t.Fatalf("tamper is a no-op: workspace_id is already %q", newWS)
	}
	claims["workspace_id"] = newWS
	nb, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	parts[1] = base64.RawURLEncoding.EncodeToString(nb)
	return strings.Join(parts, ".")
}

// TestForgery_TamperedWorkspaceClaim_RejectedByAuthenticate: a forged workspace claim breaks the
// ES256 signature, so Authenticate rejects it — the forged workspace never becomes an identity.
func TestForgery_TamperedWorkspaceClaim_RejectedByAuthenticate(t *testing.T) {
	am, key := forgeSetup(t)
	good, err := auth.GenerateToken("wsA", "u", []string{auth.ScopeProxy}, key, time.Hour)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	// Precondition: the untampered token authenticates as wsA (so the rejection below is the
	// tamper's doing, not a broken token).
	okReq := httptest.NewRequest(http.MethodGet, "/x", nil)
	okReq.Header.Set("Authorization", "Bearer "+good)
	if actx, err := am.Authenticate(okReq); err != nil || actx.WorkspaceID != "wsA" {
		t.Fatalf("precondition: valid token must authenticate as wsA (actx=%+v err=%v)", actx, err)
	}

	forged := tamperWorkspaceClaim(t, good, "wsVictim")
	fReq := httptest.NewRequest(http.MethodGet, "/x", nil)
	fReq.Header.Set("Authorization", "Bearer "+forged)
	if actx, err := am.Authenticate(fReq); err == nil {
		t.Fatalf("SECURITY: a tampered workspace claim authenticated as %q — forged identity accepted", actx.WorkspaceID)
	}
}

// TestForgery_TamperedClaim_401_NeverAttributed: through the REAL AuthMiddleware a forged token is
// 401'd BEFORE any handler runs, so it can never be rate-limited or metered under the forged ws.
func TestForgery_TamperedClaim_401_NeverAttributed(t *testing.T) {
	am, key := forgeSetup(t)
	good, err := auth.GenerateToken("wsA", "u", []string{auth.ScopeProxy}, key, time.Hour)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	forged := tamperWorkspaceClaim(t, good, "wsVictim")

	reached := false
	sink := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	h := auth.AuthMiddleware(auth.New(nil), am)(sink)

	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/x", nil)
	req.Header.Set("Authorization", "Bearer "+forged)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("forged token: status %d, want 401", rec.Code)
	}
	if reached {
		t.Fatal("SECURITY: the handler ran for a forged token — the forged workspace could be metered/attributed")
	}
}
