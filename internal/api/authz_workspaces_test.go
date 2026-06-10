package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/workspace"
)

// TestHandleWorkspaces_AdminOnly: the tenant-roster endpoint (every workspace +
// per-workspace cost/volume) must be admin-only — it is an all-tenant view with
// no single-tenant shape, so a non-admin gets 403 (its own data lives at the
// path-scoped /v1/workspaces/{wsID} routes). Part of the #146 cross-tenant
// authorization fix; RED before the gate (a non-admin received the full roster).
func TestHandleWorkspaces_AdminOnly(t *testing.T) {
	wsm := workspace.New(nil)
	if err := wsm.RegisterWorkspace(context.Background(), workspace.Workspace{ID: "ws_a", Name: "A", Active: true}); err != nil {
		t.Fatal(err)
	}
	s := &Server{wsManager: wsm}

	// Non-admin (no auth context on the request) → 403, no roster leaked.
	rec := httptest.NewRecorder()
	s.handleWorkspaces(rec, httptest.NewRequest(http.MethodGet, "/v1/api/workspaces", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin roster: got %d, want 403 (must not list all tenants)", rec.Code)
	}

	// Global admin → 200 with the roster.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/api/workspaces", nil)
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{IsAdmin: true}))
	s.handleWorkspaces(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin roster: got %d, want 200", rec.Code)
	}
	var out []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("admin roster body: %v", err)
	}
	if len(out) != 1 || out[0]["id"] != "ws_a" {
		t.Fatalf("admin roster: got %v, want the one registered workspace", out)
	}
}
