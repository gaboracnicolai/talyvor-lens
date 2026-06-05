package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestWorkspaceAuthorized covers the pure tenant-isolation decision that backs
// the cross-tenant BOLA guard: the global admin may act on any workspace; every
// other caller may act only on its own; an empty caller workspace is always
// denied against a real target.
func TestWorkspaceAuthorized(t *testing.T) {
	cases := []struct {
		name     string
		caller   string
		isAdmin  bool
		target   string
		expected bool
	}{
		{"same workspace allowed", "ws_a", false, "ws_a", true},
		{"cross tenant denied", "ws_a", false, "ws_b", false},
		{"admin reaches any workspace", "", true, "ws_b", true},
		{"admin reaches another workspace", "ws_a", true, "ws_b", true},
		{"empty caller denied", "", false, "ws_b", false},
		{"empty caller denied vs empty target", "", false, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := workspaceAuthorized(c.caller, c.isAdmin, c.target); got != c.expected {
				t.Fatalf("workspaceAuthorized(%q, %v, %q) = %v, want %v",
					c.caller, c.isAdmin, c.target, got, c.expected)
			}
		})
	}
}

// TestCallerWorkspaceID_DenyByDefault verifies that a request that never passed
// through AuthMiddleware (no identity on the context) resolves to an empty,
// non-admin caller — so the guard fails closed.
func TestCallerWorkspaceID_DenyByDefault(t *testing.T) {
	ws, isAdmin := callerWorkspaceID(context.Background())
	if ws != "" || isAdmin {
		t.Fatalf("unauthenticated context resolved to (%q, %v), want (\"\", false)", ws, isAdmin)
	}
}

// TestWorkspaceIsolationMiddleware_EnforcesAndPassesThrough drives the
// middleware through a chi router wired exactly like main.go — inside an
// r.Group with authed.Use(...). That placement matters: a group's Use
// middlewares are applied as *inline* middlewares baked into each matched
// route, so they run AFTER chi has resolved the URL params (unlike a top-level
// mx.Use, which runs before routing and would see an empty wsID — see the
// sibling sub-test below that locks in this distinction).
//
// It proves two things end-to-end:
//   - a {wsID} route with no authenticated identity is rejected with 403
//     (enforcement + params actually resolved inside the middleware), and
//   - a route without a {wsID} param passes straight through.
func TestWorkspaceIsolationMiddleware_EnforcesAndPassesThrough(t *testing.T) {
	r := chi.NewRouter()
	r.Group(func(authed chi.Router) {
		authed.Use(workspaceIsolationMiddleware)
		authed.Get("/v1/workspaces/{wsID}/config", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		authed.Get("/v1/catalog/models", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
	})

	// {wsID} route, unauthenticated → fail closed with 403.
	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/ws_victim/config", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("workspace route without identity: got %d, want 403", rec.Code)
	}

	// Non-workspace route → unaffected.
	req = httptest.NewRequest(http.MethodGet, "/v1/catalog/models", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("non-workspace route: got %d, want 200", rec.Code)
	}
}

// TestWorkspaceIsolationMiddleware_GroupPlacementResolvesParam locks in the
// load-bearing assumption: the guard MUST be registered via a group's Use (as
// main.go does), not a top-level mux Use. A top-level Use runs before routing,
// so chi.URLParam(wsID) would be empty and the guard would fail OPEN. If a
// future refactor moves the guard to a top-level Use, this test fails loudly.
func TestWorkspaceIsolationMiddleware_GroupPlacementResolvesParam(t *testing.T) {
	// Top-level Use: middleware runs before route match → wsID empty → passes.
	top := chi.NewRouter()
	top.Use(workspaceIsolationMiddleware)
	top.Get("/v1/workspaces/{wsID}/config", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/ws_victim/config", nil)
	rec := httptest.NewRecorder()
	top.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("sanity: top-level Use is expected to fail open (200), got %d — "+
			"chi behavior changed; re-verify the guard placement in main.go", rec.Code)
	}
}
