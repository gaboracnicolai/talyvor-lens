package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/taxprofile"
)

// B32.38 — the owner saves a profile with a VAT number over PUT /v1/workspaces/{ws}/tax-profile and reads it back
// with where the workspace resolves to; a member's agent key is refused, and a country that is not two letters is 400.
func TestTaxProfileRoutes(t *testing.T) {
	pool := agentRoutesDB(t)
	r := chi.NewRouter()
	mountTaxProfileRoutes(r, taxprofile.NewStore(pool, partners.NewRegistry(nil)))
	call := func(actx *auth.AuthContext, method, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, "/v1/workspaces/ws-b3238/tax-profile", strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), actx))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	owner := &auth.AuthContext{WorkspaceID: "ws-b3238", AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	agent := &auth.AuthContext{WorkspaceID: "ws-b3238", AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "agent"}

	if code, out := call(owner, http.MethodGet, ""); code != http.StatusOK || out["profile"] != nil || out["resolved"].(map[string]any)["decided_by"] != "unknown" {
		t.Fatalf("before a profile: %d %v, want 200, no profile, unknown", code, out)
	}
	if code, _ := call(owner, http.MethodPut, `{"country": "Germany"}`); code != http.StatusBadRequest {
		t.Fatalf("country Germany: %d, want 400", code)
	}
	if code, _ := call(agent, http.MethodPut, `{"country": "DE"}`); code != http.StatusForbidden {
		t.Fatalf("an agent key: %d, want 403", code)
	}
	code, out := call(owner, http.MethodPut, `{"legal_name": "Beispiel GmbH", "address": "Teststraße 1, Berlin", "country": "DE", "tax_id": "DE123456789"}`)
	if code != http.StatusOK {
		t.Fatalf("put: %d %v", code, out)
	}
	code, out = call(owner, http.MethodGet, "")
	profile, _ := out["profile"].(map[string]any)
	resolved, _ := out["resolved"].(map[string]any)
	if code != http.StatusOK || profile["tax_id_valid"] != true || resolved["country"] != "DE" || resolved["business"] != true {
		t.Fatalf("get: %d %v, want the valid VAT number and a DE business", code, out)
	}
}
