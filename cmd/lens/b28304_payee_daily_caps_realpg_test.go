package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B28.304 — the owner caps, in an agent's rules, how much it may pay one payee in a day, through the routes the
// console uses: after one payment under the cap, a second that would pass it is refused 403 and the pair has
// exactly one pay posting. The cap is the payee's alone, rules saved without it keep it, a company's cap counts
// what its agents were paid, and an empty map clears the caps.
func TestB28304_ExactlyOnePayPostingForThePairAfterASecondPaymentOverTheCapIsRefused(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-payee-caps"
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
	payer, supplier, other := newAgent("payer"), newAgent("supplier"), newAgent("other")
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
	// pay pays amount µLXC to `to` and answers the status and how many pay entries now move LXC from the payer to it.
	pay := func(to string, amount int64) (int, int) {
		t.Helper()
		code, body := call(http.MethodPost, "/v1/workspaces/"+ws+"/agents/"+payer+"/pay",
			`{"to_agent_id":"`+to+`","amount_ulxc":`+strconv.FormatInt(amount, 10)+`}`)
		if code != http.StatusOK && code != http.StatusForbidden {
			t.Fatalf("pay %s = %d %s", to, code, body)
		}
		if code == http.StatusForbidden && !strings.Contains(body, "daily limit") {
			t.Errorf("pay %s refused %s, want the payee's daily limit named", to, body)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_postings p JOIN agent_postings q ON q.entry_id = p.entry_id
			WHERE p.workspace_id = $1 AND p.kind = 'pay' AND p.account = $2 AND p.amount_ulxc < 0
			  AND q.kind = 'pay' AND q.account = $3 AND q.amount_ulxc > 0`, ws, "agent:"+payer, "agent:"+to).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return code, n
	}

	for _, bad := range []string{`{"payee_daily_limits_ulxc":{"":5}}`, `{"payee_daily_limits_ulxc":{" agt_x":5}}`,
		`{"payee_daily_limits_ulxc":{"` + supplier + `":-1}}`} {
		if code, body := call(http.MethodPut, rules, bad); code != http.StatusBadRequest {
			t.Errorf("PUT %s = %d %s, want 400", bad, code, body)
		}
	}

	put(`{"payee_daily_limits_ulxc":{"`+supplier+`":1500000}}`, `"payee_daily_limits_ulxc":{"`+supplier+`":1500000}`)
	if code, n := pay(supplier, 1_000_000); code != http.StatusOK || n != 1 {
		t.Fatalf("a first payment under the cap = %d with %d pay postings for the pair, want 200 and one", code, n)
	}
	if code, n := pay(supplier, 1_000_000); code != http.StatusForbidden || n != 1 {
		t.Fatalf("a second payment over the cap = %d with %d pay postings for the pair, want 403 and exactly one", code, n)
	}
	if code, n := pay(other, 1_000_000); code != http.StatusOK || n != 1 {
		t.Errorf("a payment to a payee with no cap = %d with %d pay postings, want 200 and one", code, n)
	}
	if code, n := pay(supplier, 500_000); code != http.StatusOK || n != 2 {
		t.Errorf("a payment that reaches the cap exactly = %d with %d pay postings for the pair, want 200 and two", code, n)
	}

	// Rules saved without the caps keep them; saved, they replace the stored ones whole.
	put(`{"daily_limit_ulxc":9000000}`, `"payee_daily_limits_ulxc":{"`+supplier+`":1500000}`)
	// A company's cap counts what its agents were paid today: 2.5 LXC so far, against a cap of 3.
	put(`{"payee_daily_limits_ulxc":{"`+ws+`":3000000}}`, `"payee_daily_limits_ulxc":{"`+ws+`":3000000}`)
	if code, n := pay(other, 1_000_000); code != http.StatusForbidden || n != 1 {
		t.Errorf("a payment past the company's cap = %d with %d pay postings, want 403 and still one", code, n)
	}
	if code, n := pay(supplier, 500_000); code != http.StatusOK || n != 3 {
		t.Errorf("a payment within the company's cap = %d with %d pay postings, want 200 and three", code, n)
	}
	// An empty map clears them.
	put(`{"payee_daily_limits_ulxc":{}}`, `"payee_daily_limits_ulxc":{}`)
	if code, n := pay(supplier, 1_000_000); code != http.StatusOK || n != 4 {
		t.Errorf("a payment once the caps are cleared = %d with %d pay postings, want 200 and four", code, n)
	}
}
