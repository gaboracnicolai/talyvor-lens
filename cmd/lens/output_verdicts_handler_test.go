package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/outputverify"
)

type fakeVerdictLister struct {
	all          []outputverify.ListedVerdict
	byWorkspace  map[string][]outputverify.ListedVerdict
	allCalled    bool
	wsCalledWith string
}

func (f *fakeVerdictLister) ListAll(_ context.Context, _ int) ([]outputverify.ListedVerdict, error) {
	f.allCalled = true
	return f.all, nil
}
func (f *fakeVerdictLister) ListForWorkspace(_ context.Context, ws string, _ int) ([]outputverify.ListedVerdict, error) {
	f.wsCalledWith = ws
	return f.byWorkspace[ws], nil
}

// The ADMIN surface is requireAdmin-gated: admin → 200; non-admin → 401, handler never reached.
func TestOutputVerdicts_AdminGate(t *testing.T) {
	serve := func(authn fakeAuthn, l *fakeVerdictLister) *httptest.ResponseRecorder {
		r := chi.NewRouter()
		r.Handle("/v1/admin/output-verdicts", requireAdmin(authn, newOutputVerdictsAdminHandler(l)))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/output-verdicts", nil))
		return rec
	}
	lister := &fakeVerdictLister{all: []outputverify.ListedVerdict{{OutputID: "oid1", WorkspaceID: "wsX", Verdict: "failed_constraint", Reason: "invalid_json"}}}
	rec := serve(fakeAuthn{ctx: &auth.AuthContext{IsAdmin: true}}, lister)
	if rec.Code != http.StatusOK || !lister.allCalled {
		t.Fatalf("admin: status=%d allCalled=%v, want 200/true", rec.Code, lister.allCalled)
	}
	if !strings.Contains(rec.Body.String(), `"verdict":"failed_constraint"`) {
		t.Errorf("admin: verdict must be visible; body=%s", rec.Body.String())
	}
	nonAdmin := &fakeVerdictLister{}
	rec = serve(fakeAuthn{ctx: &auth.AuthContext{IsAdmin: false, WorkspaceID: "wsX"}}, nonAdmin)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("non-admin: status=%d, want 401", rec.Code)
	}
	if nonAdmin.allCalled {
		t.Error("non-admin: the admin lister must NOT be reached")
	}
}

// The WORKSPACE surface returns ONLY the caller's own verdicts (scoped to the authenticated WorkspaceID);
// an unauthenticated caller (no workspace) is refused.
func TestOutputVerdicts_WorkspaceScoped(t *testing.T) {
	lister := &fakeVerdictLister{byWorkspace: map[string][]outputverify.ListedVerdict{
		"ws-alpha": {{OutputID: "a1", WorkspaceID: "ws-alpha", Verdict: "passed"}},
		"ws-beta":  {{OutputID: "b1", WorkspaceID: "ws-beta", Verdict: "failed_constraint"}},
	}}
	h := newOutputVerdictsWorkspaceHandler(fakeAuthn{ctx: &auth.AuthContext{WorkspaceID: "ws-alpha"}}, lister)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/v1/output-verdicts", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if lister.wsCalledWith != "ws-alpha" {
		t.Errorf("must scope to the caller's workspace; ListForWorkspace called with %q", lister.wsCalledWith)
	}
	if strings.Contains(rec.Body.String(), "ws-beta") || strings.Contains(rec.Body.String(), `"b1"`) {
		t.Errorf("workspace read leaked another tenant's verdicts: %s", rec.Body.String())
	}
	// Unauthenticated (no workspace resolved) → 401.
	h2 := newOutputVerdictsWorkspaceHandler(fakeAuthn{ctx: &auth.AuthContext{}}, lister)
	rec2 := httptest.NewRecorder()
	h2(rec2, httptest.NewRequest(http.MethodGet, "/v1/output-verdicts", nil))
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("no-workspace caller: status=%d, want 401", rec2.Code)
	}
}
