package workspace_test

// attribution_isolation_integration_test.go — Property 3 (attribution isolation — the moat).
//
// The proxy meters spend/COGS against wsID = workspaceManager.ExtractWorkspaceID(r)
// (internal/proxy/proxy.go:547). This proves what that resolver returns AFTER the REAL
// AuthMiddleware runs — i.e. the workspace a request's spend actually lands on:
//   - GLOBAL KEY → "default": the credential's workspace is "" (auth/manager.go:300), which
//     AuthMiddleware stamps over any client label, so every tenant's spend collapses to "default".
//   - PER-WORKSPACE JWT → its own workspace: the verified claim, never "default".
//   - FORGED header + JWT → the claim: a client X-Talyvor-Workspace cannot override the verified
//     identity (the #146 anti-spoof), so it can neither escape nor hijack attribution.

import (
	"crypto/ecdsa"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/workspace"
)

const attrGlobalKey = "test-global-admin-key-long-enough-xx"

func attrSetup(t *testing.T) (*auth.Manager, *workspace.Manager, *ecdsa.PrivateKey) {
	t.Helper()
	jwtKey, err := auth.GenerateECKey()
	if err != nil {
		t.Fatalf("GenerateECKey: %v", err)
	}
	return auth.NewManager(attrGlobalKey, jwtKey, auth.New(nil), nil), workspace.New(nil), jwtKey
}

// attrResolveThroughAuth runs a request carrying `bearer` (plus an optional forged wsLabel) through
// the REAL AuthMiddleware, then returns the workspace the proxy would attribute spend against
// (ExtractWorkspaceID), captured inside the downstream handler exactly where the proxy reads it.
func attrResolveThroughAuth(t *testing.T, am *auth.Manager, wm *workspace.Manager, bearer, wsLabel string) string {
	t.Helper()
	var attributed string
	sink := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		attributed = wm.ExtractWorkspaceID(r)
	})
	h := auth.AuthMiddleware(auth.New(nil), am)(sink)
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/x", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	if wsLabel != "" {
		req.Header.Set("X-Talyvor-Workspace", wsLabel)
	}
	h.ServeHTTP(httptest.NewRecorder(), req)
	return attributed
}

func attrMintWS(t *testing.T, key *ecdsa.PrivateKey, ws string) string {
	t.Helper()
	tok, err := auth.GenerateToken(ws, "u", []string{auth.ScopeProxy}, key, time.Hour)
	if err != nil {
		t.Fatalf("GenerateToken(%s): %v", ws, err)
	}
	return tok
}

// DEFECT: global-key traffic attributes to "default" — every tenant's spend/COGS collapses there.
func TestAttribution_GlobalKey_LandsOnDefault(t *testing.T) {
	am, wm, _ := attrSetup(t)
	if got := attrResolveThroughAuth(t, am, wm, attrGlobalKey, "wsReal"); got != "default" {
		t.Fatalf("global-key attribution = %q, want \"default\" — the wsReal label is overwritten to \"\" "+
			"(manager.go:300), so shared-key spend collapses to the default bucket", got)
	}
}

// FIX: a wsA JWT attributes to wsA — the trusted claim, never "default".
func TestAttribution_PerWorkspaceJWT_LandsOnOwnWorkspace(t *testing.T) {
	am, wm, key := attrSetup(t)
	if got := attrResolveThroughAuth(t, am, wm, attrMintWS(t, key, "wsA"), ""); got != "wsA" {
		t.Fatalf("wsA JWT attribution = %q, want wsA — JWT-authed proxy traffic must meter under its own "+
			"workspace, never default", got)
	}
}

// FORGERY (ties to #146 + property 4): a wsA JWT carrying a forged X-Talyvor-Workspace: victim
// header still attributes to wsA — AuthMiddleware overwrites the client label with the verified claim.
func TestAttribution_ForgedHeaderIgnored_UsesClaim(t *testing.T) {
	am, wm, key := attrSetup(t)
	if got := attrResolveThroughAuth(t, am, wm, attrMintWS(t, key, "wsA"), "victim-workspace"); got != "wsA" {
		t.Fatalf("attribution with a forged X-Talyvor-Workspace = %q, want wsA — a client header must NEVER "+
			"override the verified claim (#146), so it can neither escape nor hijack a victim's attribution", got)
	}
}
