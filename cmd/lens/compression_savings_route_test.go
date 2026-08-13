package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// THE NEW READ ROUTE IS DEEPER THAN THE ONES THE ISOLATION GUARD WAS WRITTEN
// AGAINST, and that difference is worth an assertion rather than an assumption.
//
// Every {wsID} route this middleware has protected so far is one segment deep —
// /v1/workspaces/{wsID}/config, /distill, /compression. The savings reader is
// TWO static segments past the parameter (/compression/savings), and the guard
// skips silently when chi.URLParam(r, "wsID") comes back empty: a path shape
// whose parameter did not resolve would fail OPEN, with no error and no log line,
// and it would serve one tenant's request volume to another.
//
// It resolves. But "chi handles that" is a claim about a library, and the cost of
// checking it is one test, while the cost of being wrong is a cross-tenant read.
//
// ⚠ THE PATH LITERAL HERE IS DELIBERATELY SPELLED OUT rather than shared with
// main.go: a constant shared with the route registration would move with it, and
// this test would keep passing against a path nobody serves.
func TestCompressionSavingsRoute_IsolationHoldsAtTheDeeperPathShape(t *testing.T) {
	r := chi.NewRouter()
	served := false
	r.Group(func(authed chi.Router) {
		authed.Use(workspaceIsolationMiddleware)
		authed.Get("/v1/workspaces/{wsID}/compression/savings", func(w http.ResponseWriter, _ *http.Request) {
			served = true
			w.WriteHeader(http.StatusOK)
		})
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/ws_victim/compression/savings?days=30", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated read of another workspace's compression savings: got %d, want 403", rec.Code)
	}
	if served {
		t.Fatal("the handler ran: {wsID} did not resolve at this path shape and the guard failed OPEN")
	}
}

// The control for the test above: the SAME router, the SAME middleware, a path
// with no {wsID} in it at all — which must pass through. Without this, a 403 that
// came from chi refusing to route (rather than from the guard refusing the
// caller) would read as a pass.
func TestCompressionSavingsRoute_TheIsolationControlPassesThrough(t *testing.T) {
	r := chi.NewRouter()
	served := false
	r.Group(func(authed chi.Router) {
		authed.Use(workspaceIsolationMiddleware)
		authed.Get("/v1/catalog/models", func(w http.ResponseWriter, _ *http.Request) {
			served = true
			w.WriteHeader(http.StatusOK)
		})
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/catalog/models", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !served {
		t.Fatalf("the no-wsID control got %d (served=%v), want 200 — the 403 above may be a routing artefact", rec.Code, served)
	}
}
