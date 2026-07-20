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
	limitSeen    int
	offsetSeen   int
}

func (f *fakeKeelLister) ListFindings(_ context.Context, _ int) ([]keel.ListedFinding, error) {
	f.called = true
	return f.rows, nil
}

func (f *fakeKeelLister) ListFindingsForWorkspacePage(_ context.Context, ws string, limit, offset int) ([]keel.ListedFinding, error) {
	f.wsCalledWith = ws
	f.limitSeen, f.offsetSeen = limit, offset
	return f.byWorkspace[ws], nil
}

// The workspace drift read passes ?limit=&offset= straight through to the store (pagination).
func TestKeelFindings_PaginationPassThrough(t *testing.T) {
	lister := &fakeKeelLister{byWorkspace: map[string][]keel.ListedFinding{"ws1": {}}}
	h := newKeelFindingsWorkspaceHandler(fakeAuthn{ctx: &auth.AuthContext{WorkspaceID: "ws1"}}, lister)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/v1/keel/findings?limit=7&offset=3", nil))
	if lister.limitSeen != 7 || lister.offsetSeen != 3 {
		t.Errorf("handler must pass query limit/offset to the store; got limit=%d offset=%d want 7/3", lister.limitSeen, lister.offsetSeen)
	}
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
