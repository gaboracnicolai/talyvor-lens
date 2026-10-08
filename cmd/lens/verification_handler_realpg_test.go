package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
)

// B30.4 — the owner climbs the verification levels through the real routes: an identity check before the email and
// phone are confirmed is 409; the email and phone check makes the workspace L1; the identity check makes it L2 —
// on the Test provider, so live money still reads L0; the record lists both checks; an agent's key is 403.
func TestVerificationRoutes_TheOwnerReachesL2OnTheTestProvider(t *testing.T) {
	pool := agentRoutesDB(t)
	r := chi.NewRouter()
	kyc := &partners.TestKYCProvider{}
	mountVerificationRoutes(r, economy.NewDualTokenStore(nil, pool, nil), func() partners.KYCProvider { return kyc })
	caller := &auth.AuthContext{WorkspaceID: "ws-b304", AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, "/v1/workspaces/ws-b304/verification"+path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), caller))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	identity := `{"name": "Ada Lovelace", "country": "GB", "date_of_birth": "1990-12-10"}`

	if code, out := call(http.MethodPost, "/identity", identity); code != http.StatusConflict {
		t.Fatalf("an identity check before L1 = %d %v; want 409", code, out)
	}
	if code, out := call(http.MethodPost, "/contact", `{"email": "ada@example.com"}`); code != http.StatusBadRequest {
		t.Fatalf("an email and phone check without the phone = %d %v; want 400", code, out)
	}
	code, out := call(http.MethodPost, "/contact", `{"email": "ada@example.com", "phone": "+447700900123"}`)
	if v, _ := out["verification"].(map[string]any); code != http.StatusCreated || v["level"] != "L1" {
		t.Fatalf("the email and phone check = %d %v; want 201 at L1", code, out)
	}
	code, out = call(http.MethodPost, "/identity", identity)
	check, _ := out["check"].(map[string]any)
	if v, _ := out["verification"].(map[string]any); code != http.StatusCreated || v["level"] != "L2" || v["live_level"] != "L0" ||
		check["status"] != "completed" || check["test"] != true || check["evidence_ref"] == "" || check["verified_name"] != "Ada Lovelace" {
		t.Fatalf("the identity check = %d %v; want 201 at L2, live L0, passed by the Test provider", code, out)
	}
	code, out = call(http.MethodGet, "", "")
	if checks, _ := out["checks"].([]any); code != http.StatusOK || out["level"] != "L2" || len(checks) != 2 {
		t.Fatalf("the record = %d %v; want L2 and both checks", code, out)
	}

	caller = &auth.AuthContext{WorkspaceID: "ws-b304", AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "agent"}
	if code, _ := call(http.MethodGet, "", ""); code != http.StatusForbidden {
		t.Fatalf("an agent's key reading the verification = %d; want 403", code)
	}
}
