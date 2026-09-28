package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
)

// B19.14 — an agent whose rules name listing A is served through A and refused through listing B: 403,
// the model never called, nothing recorded or billed. (A use is answered buffered; there is no streamed use.)
func TestMarketUse_AnAgentUsesOnlyTheListingsItsRulesName(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const seller, buyer = "ws-seller", "ws-buyer"
	stripeFake := &marketStripe{}
	store := market.NewStore(pool)
	bank := economy.NewDualTokenStore(nil, pool, nil)
	svc := billing.New(pool, bank, stripeFake, "whsec_test").WithMarketBill(stripeFake, "price_market", "talyvor_marketplace_use", store)
	called := 0
	lens := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called++
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`))
	})
	r := chi.NewRouter()
	mountMarketRoutes(r, store)
	mountMarketUseRoutes(r, store, lens, svc, bank)
	call := func(who *auth.AuthContext, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tlv_x")
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	sellerOwner := &auth.AuthContext{WorkspaceID: seller, AuthMethod: auth.MethodJWT, UserID: "s", Scopes: []string{auth.ScopeKeys}}
	listing := func(title string) string {
		t.Helper()
		code, body := call(sellerOwner, http.MethodPost, "/v1/workspaces/"+seller+"/marketplace/listings",
			`{"kind":"skill","title":"`+title+`","price_per_use_ulxc":20000,"artifact":{"instructions":"Chart it.","model":"gpt-5-mini"}}`)
		var l market.Listing
		if err := json.Unmarshal([]byte(body), &l); err != nil || code != http.StatusCreated {
			t.Fatalf("publish %s = %d %s", title, code, body)
		}
		return l.ID
	}
	a, b := listing("Charts A"), listing("Charts B")

	agent, err := bank.CreateAgent(ctx, buyer, "analyst", "owner-b")
	if err != nil {
		t.Fatal(err)
	}
	if err := bank.AttachAgentKey(ctx, buyer, agent.ID, "key-analyst"); err != nil {
		t.Fatal(err)
	}
	if _, err := bank.SetAgentRules(ctx, buyer, agent.ID, economy.AgentRules{AllowedListings: []string{a}}); err != nil {
		t.Fatal(err)
	}
	if got, err := bank.GetAgentRules(ctx, buyer, agent.ID); err != nil || len(got.AllowedListings) != 1 || got.AllowedListings[0] != a {
		t.Fatalf("the rules read back as %+v (%v)", got.AllowedListings, err)
	}
	// Saving its other rules without naming listings (a client that predates them) keeps them.
	if _, err := bank.SetAgentRules(ctx, buyer, agent.ID, economy.AgentRules{MaxPerRequestULXC: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	if got, _ := bank.GetAgentRules(ctx, buyer, agent.ID); len(got.AllowedListings) != 1 || got.MaxPerRequestULXC != 1_000_000 {
		t.Fatalf("after saving the other rules, allowed listings = %v, max per request %d", got.AllowedListings, got.MaxPerRequestULXC)
	}
	key := &auth.AuthContext{WorkspaceID: buyer, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "key-analyst", Scopes: []string{auth.ScopeProxy}}
	use := func(id string) (int, string) {
		return call(key, http.MethodPost, "/v1/workspaces/"+buyer+"/marketplace/listings/"+id+"/use", `{"input":"Q3 revenue by region"}`)
	}

	if code, body := use(a); code != http.StatusOK || called != 1 || len(stripeFake.events) != 1 {
		t.Fatalf("the agent through listing A = %d %s (model called %d, billed %d), want served and billed once", code, body, called, len(stripeFake.events))
	}
	code, body := use(b)
	if code != http.StatusForbidden || !strings.Contains(body, "may not use the marketplace listing") {
		t.Errorf("the agent through listing B = %d %s, want 403", code, body)
	}
	var recorded int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM market_uses WHERE listing_id = $1`, b).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if called != 1 || len(stripeFake.events) != 1 || recorded != 0 {
		t.Errorf("after the refusal: model called %d times, %d meter events, %d uses of B recorded; want 1, 1, 0", called, len(stripeFake.events), recorded)
	}
	// A person of the same workspace is not the agent: their use of B goes through.
	person := &auth.AuthContext{WorkspaceID: buyer, AuthMethod: auth.MethodJWT, UserID: "owner-b", Scopes: []string{auth.ScopeKeys}}
	if code, body := call(person, http.MethodPost, "/v1/workspaces/"+buyer+"/marketplace/listings/"+b+"/use", `{"input":"x"}`); code != http.StatusOK {
		t.Errorf("a person's use of listing B = %d %s", code, body)
	}
}
