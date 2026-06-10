package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/talyvor/lens/internal/auth"
)

// reqAs builds a request whose context carries the given authenticated identity,
// exactly as AuthMiddleware would: a non-admin DB key populates the APIKey slot
// (auth.GetAPIKey), while the global admin key / a JWT populates the AuthContext
// slot (auth.GetAuthContext, the only one that carries IsAdmin).
func reqAs(workspace string, admin bool) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := r.Context()
	switch {
	case admin:
		ctx = auth.WithAuthContext(ctx, &auth.AuthContext{WorkspaceID: workspace, IsAdmin: true})
	case workspace != "":
		ctx = auth.WithAPIKey(ctx, &auth.APIKey{WorkspaceID: workspace})
	}
	return r.WithContext(ctx)
}

// TestEffectiveWorkspaceID is the Phase-1 authorization chokepoint guard
// (umbrella #146). Every fixed route funnels its caller-supplied workspace input
// through this function; each case models a real route's input channel. The
// predicted cross-tenant failure is a non-admin caller "ws_a" naming victim
// "ws_b" and being forced back to its OWN workspace — never reaching ws_b.
func TestEffectiveWorkspaceID(t *testing.T) {
	cases := []struct {
		name      string
		callerWS  string
		admin     bool
		requested string
		wantWS    string
		wantAdmin bool
		wantOK    bool
	}{
		// The leak, per Phase-1 route: a tenant naming another tenant is forced to its own.
		{"POST /v1/api/keys: ws_a body workspace_id=ws_b -> ws_a (takeover blocked)", "ws_a", false, "ws_b", "ws_a", false, true},
		{"GET /v1/audit/export: ws_a query ws_b -> ws_a (no cross-tenant log)", "ws_a", false, "ws_b", "ws_a", false, true},
		{"GET /v1/audit/export: ws_a query EMPTY -> ws_a (never all-tenants)", "ws_a", false, "", "ws_a", false, true},
		{"POST /v1/audit/webhook: ws_a filter ws_b -> ws_a", "ws_a", false, "ws_b", "ws_a", false, true},
		{"PUT /v1/guardrails/policy: ws_a body ws_b -> ws_a", "ws_a", false, "ws_b", "ws_a", false, true},
		{"PUT /v1/prompts/{name}: ws_a body ws_b -> ws_a", "ws_a", false, "ws_b", "ws_a", false, true},
		{"POST /v1/prompts/{name}/rollback: ws_a body ws_b -> ws_a", "ws_a", false, "ws_b", "ws_a", false, true},
		{"GET /v1/marketplace/trades: ws_a query ws_b -> ws_a (#144)", "ws_a", false, "ws_b", "ws_a", false, true},
		{"POST /v1/marketplace/listings: ws_a seller_id=ws_b -> ws_a", "ws_a", false, "ws_b", "ws_a", false, true},
		{"POST /v1/marketplace/listings/{id}/buy: ws_a buyer_workspace=ws_b -> ws_a", "ws_a", false, "ws_b", "ws_a", false, true},
		{"DELETE /v1/marketplace/listings/{id}: ws_a query ws_b -> ws_a", "ws_a", false, "ws_b", "ws_a", false, true},

		// Honest non-admin naming its OWN workspace is unaffected (no regression).
		{"honest non-admin: ws_a requests ws_a -> ws_a", "ws_a", false, "ws_a", "ws_a", false, true},

		// Admin honors the requested param (cross-tenant admin reads preserved).
		{"admin honors requested ws_b", "ws_admin", true, "ws_b", "ws_b", true, true},
		{"admin EMPTY preserves admin-wide semantics (e.g. all-tenant export)", "", true, "", "", true, true},

		// Fail closed: a non-admin with no resolvable workspace is denied (403), never a fallthrough.
		{"empty caller non-admin -> deny", "", false, "ws_b", "", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotWS, gotAdmin, gotOK := effectiveWorkspaceID(reqAs(c.callerWS, c.admin), c.requested)
			if gotWS != c.wantWS || gotAdmin != c.wantAdmin || gotOK != c.wantOK {
				t.Fatalf("effectiveWorkspaceID(caller=%q admin=%v, requested=%q) = (%q,%v,%v), want (%q,%v,%v)",
					c.callerWS, c.admin, c.requested, gotWS, gotAdmin, gotOK, c.wantWS, c.wantAdmin, c.wantOK)
			}
		})
	}
}
