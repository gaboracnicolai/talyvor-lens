package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/auth"
)

// A3L: /mcp exposed per-workspace financial telemetry with NO auth + arg-trust. The mount must reject
// unauthenticated calls (it was bare), and tools must force the acted-on workspace to the verified
// caller — never the caller-supplied arg.
func TestMCP_MountRequiresAuth(t *testing.T) {
	srv := New(nil, nil, nil, nil, nil, "test")
	ks := auth.New(nil)
	mgr := auth.NewManager("admin-secret-key-0123456789", nil, ks, nil)
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize"}`
	newReq := func() *http.Request { return httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body)) }

	// TODAY (bare mount, as main.go had it): an unauthenticated call reaches HandleRPC — NOT rejected.
	bare := httptest.NewRecorder()
	http.HandlerFunc(srv.HandleRPC).ServeHTTP(bare, newReq())
	if bare.Code == http.StatusUnauthorized {
		t.Fatalf("premise broken: bare /mcp already 401s (%d)", bare.Code)
	}

	// FIX: behind AuthMiddleware, an unauthenticated call is rejected 401 before any tool runs.
	guarded := httptest.NewRecorder()
	auth.AuthMiddleware(ks, mgr)(http.HandlerFunc(srv.HandleRPC)).ServeHTTP(guarded, newReq())
	if guarded.Code != http.StatusUnauthorized {
		t.Errorf("guarded /mcp unauthenticated = %d, want 401 (no credential)", guarded.Code)
	}
}

// The tools must derive the acted-on workspace from the VERIFIED credential, never the arg.
func TestMCP_EffectiveWorkspace_ForcesNonAdmin(t *testing.T) {
	// non-admin verified as ws-A tries to target ws-B via the arg → forced to ws-A.
	ctxA := auth.WithAuthContext(context.Background(), &auth.AuthContext{WorkspaceID: "ws-A", IsAdmin: false})
	if got := effectiveWorkspace(ctxA, "ws-B"); got != "ws-A" {
		t.Errorf("non-admin arg-trust: effectiveWorkspace(ws-A, arg=ws-B) = %q, want ws-A — the arg must be ignored", got)
	}
	// admin may target any workspace via the arg.
	ctxAdmin := auth.WithAuthContext(context.Background(), &auth.AuthContext{IsAdmin: true})
	if got := effectiveWorkspace(ctxAdmin, "ws-B"); got != "ws-B" {
		t.Errorf("admin: effectiveWorkspace(admin, arg=ws-B) = %q, want ws-B", got)
	}
	// unauthenticated context → "" (fail closed).
	if got := effectiveWorkspace(context.Background(), "ws-B"); got != "" {
		t.Errorf("unauth: effectiveWorkspace = %q, want empty (fail closed)", got)
	}
}
