package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B28.303 — the owner names, in an agent's rules, who it may pay and who it may not, through the routes the
// console uses: a payment to a blocked payee is refused 403 and posts nothing; once the rules name allowed
// payees, a payment to one posts its pair and a payment to any other posts nothing. Rules saved without the
// lists keep them, an empty list clears one, and a payee both allowed and blocked is refused 400.
func TestB28303_NoPayPostingToABlockedPayee_OneToAnAllowedPayee(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-payees"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 20000000, 20000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	newAgent := func(name string) string {
		t.Helper()
		a, err := store.CreateAgent(ctx, ws, name, "user-owner")
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	payer, supplier, blocked, stranger := newAgent("payer"), newAgent("supplier"), newAgent("blocked"), newAgent("stranger")
	if _, err := store.FundAgent(ctx, ws, payer, 10_000_000); err != nil {
		t.Fatal(err)
	}
	rules := "/v1/workspaces/" + ws + "/agents/" + payer + "/rules"
	put := func(body, want string) {
		t.Helper()
		if code, got := call(http.MethodPut, rules, body); code != http.StatusOK || !strings.Contains(got, want) {
			t.Fatalf("PUT rules %s = %d %s, want %s", body, code, got, want)
		}
		if _, got := call(http.MethodGet, rules, ""); !strings.Contains(got, want) {
			t.Fatalf("after PUT %s, GET rules = %s, want %s", body, got, want)
		}
	}
	// pay pays 1 LXC to `to` and answers the status and how many 'pay' postings now credit it.
	pay := func(to string) (int, int) {
		t.Helper()
		code, body := call(http.MethodPost, "/v1/workspaces/"+ws+"/agents/"+payer+"/pay", `{"to_agent_id":"`+to+`","amount_ulxc":1000000}`)
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_postings WHERE workspace_id = $1 AND account = $2 AND kind = 'pay' AND amount_ulxc > 0`,
			ws, "agent:"+to).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if code != http.StatusOK && code != http.StatusForbidden {
			t.Fatalf("pay %s = %d %s", to, code, body)
		}
		return code, n
	}

	if code, body := call(http.MethodPut, rules, `{"allowed_payees":["`+blocked+`"],"blocked_payees":["`+blocked+`"]}`); code != http.StatusBadRequest {
		t.Errorf("a payee both allowed and blocked = %d %s, want 400", code, body)
	}

	put(`{"blocked_payees":["`+blocked+`"]}`, `"blocked_payees":["`+blocked+`"]`)
	if code, n := pay(blocked); code != http.StatusForbidden || n != 0 {
		t.Errorf("paying a blocked payee = %d with %d pay postings to it, want 403 and none", code, n)
	}
	if code, n := pay(stranger); code != http.StatusOK || n != 1 {
		t.Errorf("paying a payee no list names, with only a block list = %d with %d pay postings, want 200 and one", code, n)
	}

	// Rules saved without the lists keep them; the allowed list then admits only whom it names.
	put(`{"daily_limit_ulxc":9000000}`, `"blocked_payees":["`+blocked+`"]`)
	put(`{"allowed_payees":["`+supplier+`"]}`, `"allowed_payees":["`+supplier+`"],"blocked_payees":["`+blocked+`"]`)
	if code, n := pay(supplier); code != http.StatusOK || n != 1 {
		t.Errorf("paying an allowed payee = %d with %d pay postings to it, want 200 and exactly one", code, n)
	}
	if code, n := pay(stranger); code != http.StatusForbidden || n != 1 {
		t.Errorf("paying a payee the allowed list does not name = %d with %d pay postings to it, want 403 and still one", code, n)
	}
	if code, n := pay(blocked); code != http.StatusForbidden || n != 0 {
		t.Errorf("paying a blocked payee = %d with %d pay postings to it, want 403 and none", code, n)
	}

	// Empty lists clear them.
	put(`{"allowed_payees":[],"blocked_payees":[]}`, `"allowed_payees":[],"blocked_payees":[]`)
	if code, n := pay(blocked); code != http.StatusOK || n != 1 {
		t.Errorf("paying the once-blocked payee after the lists are cleared = %d with %d pay postings, want 200 and one", code, n)
	}
}
