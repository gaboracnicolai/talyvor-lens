package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/keel"
)

type fakeKeelLister struct {
	rows         []keel.ListedFinding
	called       bool
	byWorkspace  map[string][]keel.ListedFinding
	wsCalledWith string
}

func (f *fakeKeelLister) ListFindings(_ context.Context, _ int) ([]keel.ListedFinding, error) {
	f.called = true
	return f.rows, nil
}

func (f *fakeKeelLister) ListFindingsForWorkspace(_ context.Context, ws string, _ int) ([]keel.ListedFinding, error) {
	f.wsCalledWith = ws
	return f.byWorkspace[ws], nil
}

// The keel drift-findings read surface is requireAdmin-gated: a tenant must NEVER read another tenant's
// drift attribution. (a) admin → 200 + rows; (c) non-admin → 401, handler never reached (fail closed).
func TestKeelFindings_AdminGate(t *testing.T) {
	serve := func(authn fakeAuthn, l *fakeKeelLister) *httptest.ResponseRecorder {
		r := chi.NewRouter()
		r.Handle("/v1/admin/keel/findings", requireAdmin(authn, newKeelFindingsHandler(l)))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/keel/findings", nil))
		return rec
	}

	// (a) admin → 200, the finding is visible.
	lister := &fakeKeelLister{rows: []keel.ListedFinding{{WorkspaceID: "wsA", Unit: "openai/gpt-4o", Attribution: keel.AttributionIdiosyncratic, DeviationSigma: -3.2, CohortN: 4}}}
	rec := serve(fakeAuthn{ctx: &auth.AuthContext{IsAdmin: true}}, lister)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin: status=%d, want 200", rec.Code)
	}
	if !lister.called {
		t.Error("admin: the lister must be reached")
	}
	if !strings.Contains(rec.Body.String(), `"attribution":"idiosyncratic"`) {
		t.Errorf("admin: finding must be visible; body=%s", rec.Body.String())
	}

	// (c) non-admin → 401, handler never reached.
	nonAdmin := &fakeKeelLister{}
	rec = serve(fakeAuthn{ctx: &auth.AuthContext{IsAdmin: false, WorkspaceID: "wsA"}}, nonAdmin)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("non-admin: status=%d, want 401", rec.Code)
	}
	if nonAdmin.called {
		t.Error("non-admin: the lister must NOT be reached (refused before the handler)")
	}
}
