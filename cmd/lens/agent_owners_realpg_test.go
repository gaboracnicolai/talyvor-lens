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
	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B19.11 — an agent's record names its owner, the verified badge follows the owner's verification, and
// an agent with no owner is refused a balance until a person claims it.
func TestAgentRoutes_EveryAgentIsTiedToAVerifiedOwner(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-agents"
	for _, q := range []string{
		`INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ('ws-agents', 20000000, 20000000)`,
		`INSERT INTO workspaces (id, name, cache_prefix) VALUES ('ws-agents', 'ws-agents', 'ws-agents')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetOwnerVerifier(earnverify.New(false))
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	nicolai := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "user-nicolai", Scopes: []string{auth.ScopeKeys}}
	admin := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodGlobalKey, IsAdmin: true}
	call := func(who *auth.AuthContext, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	base := "/v1/workspaces/" + ws + "/agents"
	agents := func() map[string]economy.Agent {
		t.Helper()
		_, body := call(nicolai, http.MethodGet, base, "")
		var book economy.AgentBook
		if err := json.Unmarshal([]byte(body), &book); err != nil {
			t.Fatal(err)
		}
		out := map[string]economy.Agent{}
		for _, a := range book.Agents {
			out[a.Name] = a
		}
		return out
	}

	// Whoever creates an agent owns it; an admin credential must name the owner.
	if code, body := call(nicolai, http.MethodPost, base, `{"name":"researcher"}`); code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	if code, _ := call(admin, http.MethodPost, base, `{"name":"nobody's"}`); code != http.StatusBadRequest {
		t.Errorf("an admin created an agent with no owner: %d, want 400", code)
	}
	if code, body := call(admin, http.MethodPost, base, `{"name":"writer","owner_user_id":"user-ana"}`); code != http.StatusCreated {
		t.Fatalf("admin create for user-ana = %d %s", code, body)
	}
	a := agents()
	if a["researcher"].OwnerUserID != "user-nicolai" || a["writer"].OwnerUserID != "user-ana" {
		t.Errorf("owners = %q and %q, want user-nicolai and user-ana", a["researcher"].OwnerUserID, a["writer"].OwnerUserID)
	}

	// The badge follows the owner's verification: off, on when Talyvor vouches, off again when it stops.
	for _, step := range []struct {
		vouched, want bool
	}{{false, false}, {true, true}, {false, false}} {
		if _, err := pool.Exec(ctx, `UPDATE workspaces SET earn_verified = $1 WHERE id = $2`, step.vouched, ws); err != nil {
			t.Fatal(err)
		}
		if got := agents()["researcher"].Verified; got != step.want {
			t.Errorf("with the owner's workspace vouched=%t the researcher's badge is %t, want %t", step.vouched, got, step.want)
		}
	}

	// An agent from before owners were recorded holds nothing until a person claims it: funding it and
	// paying it are refused, and nothing moves.
	if _, err := pool.Exec(ctx, `INSERT INTO agent_accounts (id, workspace_id, name) VALUES ('agt_legacy', $1, 'legacy')`, ws); err != nil {
		t.Fatal(err)
	}
	researcher := a["researcher"].ID
	if code, body := call(nicolai, http.MethodPost, base+"/"+researcher+"/fund", `{"amount_ulxc":3000000}`); code != http.StatusOK {
		t.Fatalf("fund the researcher = %d %s", code, body)
	}
	if code, body := call(nicolai, http.MethodPost, base+"/agt_legacy/fund", `{"amount_ulxc":1000000}`); code != http.StatusConflict || !strings.Contains(body, "no owner") {
		t.Errorf("funding the owner-less agent = %d %s, want 409", code, body)
	}
	if code, body := call(nicolai, http.MethodPost, base+"/"+researcher+"/pay", `{"to_agent_id":"agt_legacy","amount_ulxc":1000000}`); code != http.StatusConflict {
		t.Errorf("paying the owner-less agent = %d %s, want 409", code, body)
	}
	if got := agents(); got["legacy"].BalanceULXC != 0 || got["legacy"].Verified || got["researcher"].BalanceULXC != 3_000_000 {
		t.Errorf("after the refusals: legacy %+v, researcher %+v", got["legacy"], got["researcher"])
	}
	// Claimed, it is owned and can be funded.
	if code, body := call(nicolai, http.MethodPost, base+"/agt_legacy/claim", ``); code != http.StatusOK {
		t.Fatalf("claim = %d %s", code, body)
	}
	if code, body := call(nicolai, http.MethodPost, base+"/agt_legacy/fund", `{"amount_ulxc":1000000}`); code != http.StatusOK {
		t.Errorf("funding the claimed agent = %d %s", code, body)
	}
	if got := agents()["legacy"]; got.OwnerUserID != "user-nicolai" || got.BalanceULXC != 1_000_000 {
		t.Errorf("the claimed agent = %+v, want owned by user-nicolai holding 1,000,000", got)
	}
}
