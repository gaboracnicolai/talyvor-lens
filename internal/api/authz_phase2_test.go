package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/talyvor/lens/internal/auth"
)

// authz_phase2_test.go — the /v1/api/* dashboard CHOKEPOINT guard (#146 Phase 2).
// All 10 dashboard read handlers derive their workspace from s.effectiveWorkspaceID
// (verified statically: it is the only reader of the workspace_id param), so the
// behavioral guarantee for every one of them is proven once, here.

func apiReq(target, workspace string, admin bool) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	ctx := r.Context()
	switch {
	case admin:
		ctx = auth.WithAuthContext(ctx, &auth.AuthContext{WorkspaceID: workspace, IsAdmin: true})
	case workspace != "":
		ctx = auth.WithAPIKey(ctx, &auth.APIKey{WorkspaceID: workspace})
	}
	return r.WithContext(ctx)
}

func TestEffectiveWorkspaceID_DashboardChokepoint(t *testing.T) {
	s := &Server{}
	cases := []struct {
		name     string
		target   string
		callerWS string
		admin    bool
		wantWS   string
		wantOK   bool
	}{
		{"non-admin attacking ws-B → forced to own", "/x?workspace_id=ws-B", "ws-A", false, "ws-A", true},
		{"non-admin, NO param → own, NEVER the shared default", "/x", "ws-A", false, "ws-A", true},
		{"non-admin honest own param", "/x?workspace_id=ws-A", "ws-A", false, "ws-A", true},
		{"admin honors the param", "/x?workspace_id=ws-B", "ws-adm", true, "ws-B", true},
		{"admin, no param → the historical default", "/x", "ws-adm", true, "default", true},
		{"unauthenticated → deny (fail closed)", "/x?workspace_id=ws-B", "", false, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotWS, gotOK := s.effectiveWorkspaceID(apiReq(c.target, c.callerWS, c.admin))
			if gotWS != c.wantWS || gotOK != c.wantOK {
				t.Fatalf("effectiveWorkspaceID = (%q,%v), want (%q,%v)", gotWS, gotOK, c.wantWS, c.wantOK)
			}
		})
	}
}
