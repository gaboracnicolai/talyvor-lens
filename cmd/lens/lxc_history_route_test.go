package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
)

// lxc/history is a {wsID} route registered on the authed group, so its cross-tenant refusal is the SAME
// mechanism as tokens/history and lxc/balance: workspaceIsolationMiddleware binds {wsID} to the caller's
// credential and 403s a foreign wsID (admin bypasses). (This 403 — rather than a 404 — is the deliberate
// consequence of mirroring the {wsID} fiat-read neighbours; the self-scoped bond/attribution reads, which
// take no {wsID}, 404 on a foreign id instead.)
func TestLXCHistoryRoute_CrossTenantRefused(t *testing.T) {
	r := chi.NewRouter()
	r.Group(func(authed chi.Router) {
		authed.Use(workspaceIsolationMiddleware)
		authed.Get("/v1/workspaces/{wsID}/lxc/history", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
	})
	serve := func(callerWS, targetWS string) int {
		req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+targetWS+"/lxc/history", nil)
		req = req.WithContext(auth.WithAuthContext(context.Background(), &auth.AuthContext{WorkspaceID: callerWS}))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := serve("wsA", "wsB"); code != http.StatusForbidden {
		t.Errorf("cross-tenant lxc/history: got %d, want 403 (workspaceIsolationMiddleware)", code)
	}
	if code := serve("wsA", "wsA"); code != http.StatusOK {
		t.Errorf("own-workspace lxc/history: got %d, want 200", code)
	}
	// Admin bypasses isolation, exactly like the sibling {wsID} fiat reads.
	adminReq := httptest.NewRequest(http.MethodGet, "/v1/workspaces/wsB/lxc/history", nil)
	adminReq = adminReq.WithContext(auth.WithAuthContext(context.Background(), &auth.AuthContext{WorkspaceID: "wsA", IsAdmin: true}))
	adminRec := httptest.NewRecorder()
	r.ServeHTTP(adminRec, adminReq)
	if adminRec.Code != http.StatusOK {
		t.Errorf("admin lxc/history any workspace: got %d, want 200", adminRec.Code)
	}
}
