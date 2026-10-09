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
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
)

// B30.13 — on test money a company opens GBP and EUR accounts and an agent a GBP account, each at zero on the
// ledger; a live open without a clearance is refused naming the class, and opens nothing.
func TestMoneyAccountRoutes_ACompanyAndItsAgentOpenAccountsAtZero(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b3013-accounts"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(partners.NewRegistry(nil))
	agent, err := store.CreateAgent(ctx, ws, "Treasurer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	mountMoneyAccountRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, "/v1/money/accounts", strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}

	opened := map[string]map[string]any{}
	for name, body := range map[string]string{"company GBP": `{"currency":"GBP"}`, "company EUR": `{"currency":"eur","funding":"test"}`} {
		code, out := call(http.MethodPost, body)
		if code != http.StatusCreated || out["purpose"] != "company" || out["partner_account_ref"] == "" {
			t.Fatalf("open %s = %d %v, want 201, a company account at the partner", name, code, out)
		}
		opened[name] = out
	}
	code, out := call(http.MethodPost, `{"currency":"GBP","agent_id":"`+agent.ID+`"}`)
	if code != http.StatusCreated || out["agent_id"] != agent.ID || out["parent_account_id"] != opened["company GBP"]["id"] {
		t.Fatalf("open the agent's GBP = %d %v, want 201, a sub-account of the company's GBP account %s", code, out, opened["company GBP"]["id"])
	}
	opened["agent GBP"] = out
	if code, out := call(http.MethodPost, `{"currency":"GBP"}`); code != http.StatusConflict {
		t.Errorf("open the company's GBP again = %d %v, want 409", code, out)
	}

	// Each is at zero on the ledger: no posting names it, and its stored balance reads zero.
	for name, a := range opened {
		id := a["id"].(string)
		var postings int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_postings WHERE account_id = $1`, id).Scan(&postings); err != nil {
			t.Fatal(err)
		}
		b, err := store.Balance(ctx, ws, id)
		if err != nil || postings != 0 || b.AmountMinor != 0 {
			t.Errorf("%s %s: %d postings, balance %+v (%v), want none and zero", name, id, postings, b, err)
		}
	}
	// The company's accounts are mirrored by partner accounts under the same reference, for reconciliation (B30.11).
	var mirrored int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_accounts p JOIN money_accounts c ON c.partner_account_ref = p.partner_account_ref
		WHERE p.purpose = 'partner' AND c.purpose = 'company' AND c.workspace_id = $1`, ws).Scan(&mirrored); err != nil || mirrored != 2 {
		t.Errorf("%d company accounts have a partner account under their reference (%v), want 2", mirrored, err)
	}
	if code, out := call(http.MethodGet, ""); code != http.StatusOK || len(out["accounts"].([]any)) != 3 {
		t.Fatalf("list = %d %v, want the three accounts", code, out)
	}

	// Live money: currency_accounts is RED and uncleared, so the open is refused naming the class, and nothing opens.
	code, out = call(http.MethodPost, `{"currency":"USD","funding":"live"}`)
	if msg, _ := out["error"].(string); code != http.StatusForbidden || !strings.Contains(msg, "class RED") {
		t.Fatalf("a live USD open = %d %v, want 403 naming class RED", code, out)
	}
	var usd int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_accounts WHERE workspace_id = $1 AND currency = 'USD'`, ws).Scan(&usd); err != nil || usd != 0 {
		t.Errorf("the refused live open left %d USD accounts (%v), want none", usd, err)
	}
}
