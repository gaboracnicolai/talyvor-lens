package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	capabilityterms "github.com/talyvor/lens/docs/terms"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// B30.9 — the owner reads a capability's terms and accepts them through the real routes, as Lens publishes them when
// it starts: every money capability is listed unaccepted; the fx text is the draft; the operator may not accept for
// the workspace (403), nor anyone a version that is not the latest (409); the owner's acceptance is 201 and listed.
func TestCapabilityTermsRoutes_TheOwnerReadsAndAcceptsTheFXTerms(t *testing.T) {
	pool := agentRoutesDB(t)
	store := economy.NewDualTokenStore(nil, pool, nil)
	if _, err := store.PublishFirstTerms(context.Background(), economy.CapabilityTermsTexts(capabilityterms.FS)); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	mountCapabilityTermsRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: "ws-b309", AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}
	call := func(who *auth.AuthContext, method, path, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, "/v1/workspaces/ws-b309/terms"+path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	listed := func() map[string]map[string]any {
		code, out := call(owner, http.MethodGet, "", "")
		terms, _ := out["terms"].([]any)
		if code != http.StatusOK || len(terms) != 19 {
			t.Fatalf("the terms list = %d with %d; want 200 with the 19 money capabilities", code, len(terms))
		}
		byKey := map[string]map[string]any{}
		for _, x := range terms {
			m, _ := x.(map[string]any)
			byKey[m["capability"].(string)] = m
		}
		return byKey
	}

	if fx := listed()["fx"]; fx["version"] != float64(1) || fx["accepted"] != nil || fx["name"] != "Converting between currencies" {
		t.Fatalf("fx in the list = %v; want version 1, not accepted", fx)
	}
	code, out := call(owner, http.MethodGet, "/fx", "")
	if body, _ := out["body"].(string); code != http.StatusOK || !strings.HasPrefix(body, economy.TermsDraftHeading) {
		t.Fatalf("the fx terms = %d %v; want 200 with the draft text", code, out)
	}
	operator := &auth.AuthContext{IsAdmin: true, AuthMethod: auth.MethodJWT}
	if code, out := call(operator, http.MethodPost, "/fx/accept", `{"version": 1}`); code != http.StatusForbidden {
		t.Fatalf("the operator accepting = %d %v; want 403", code, out)
	}
	if code, out := call(owner, http.MethodPost, "/fx/accept", `{"version": 2}`); code != http.StatusConflict {
		t.Fatalf("accepting a version that is not the latest = %d %v; want 409", code, out)
	}
	code, out = call(owner, http.MethodPost, "/fx/accept", `{"version": 1}`)
	if a, _ := out["acceptance"].(map[string]any); code != http.StatusCreated || a["person"] != "jwt:user:ada" || a["version"] != float64(1) {
		t.Fatalf("the owner accepting = %d %v; want 201 naming the owner", code, out)
	}
	if fx := listed()["fx"]; fx["accepted"] == nil {
		t.Fatalf("fx after the owner accepted = %v; want it accepted", fx)
	}
}
