package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
)

// B30.13 — an agent opens its own GBP account with its own key, a sub-account of its company's, and lists it at zero.
func TestWalletAccounts_AnAgentOpensItsOwnAccount(t *testing.T) {
	pool := savingsTestPool(t)
	ctx := context.Background()
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(partners.NewRegistry(nil))
	a, err := store.CreateAgent(ctx, "ws-b3013-mcp", "Payer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachAgentKey(ctx, "ws-b3013-mcp", a.ID, "key-payer"); err != nil {
		t.Fatal(err)
	}
	company, err := store.OpenCurrencyAccount(ctx, "ws-b3013-mcp", "", "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer(pool, nil, nil, nil, nil, "test")
	srv.SetAgentBank(store)
	call := func(name, args string) (bool, string) {
		t.Helper()
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: "key-payer", WorkspaceID: "ws-b3013-mcp"}))
		w := httptest.NewRecorder()
		srv.HandleRPC(w, req)
		var resp struct {
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || len(resp.Result.Content) == 0 {
			t.Fatalf("%s = %s", name, w.Body.String())
		}
		return resp.Result.IsError, resp.Result.Content[0].Text
	}

	if refused, text := call("wallet_account_open", `{"currency":"EUR"}`); !refused || !strings.Contains(text, "company's account in this currency first") {
		t.Fatalf("wallet_account_open EUR before the company's = %s; want refused, saying the company's opens first", text)
	}
	refused, text := call("wallet_account_open", `{"currency":"GBP"}`)
	var opened economy.CurrencyAccount
	if err := json.Unmarshal([]byte(text), &opened); refused || err != nil || opened.AgentID != a.ID || opened.ParentAccountID != company.ID {
		t.Fatalf("wallet_account_open GBP = %s; want the agent's own account under the company's %s", text, company.ID)
	}
	refused, text = call("wallet_accounts", `{}`)
	var list struct {
		Accounts []economy.CurrencyAccount `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(text), &list); refused || err != nil || len(list.Accounts) != 1 || list.Accounts[0].ID != opened.ID ||
		list.Accounts[0].BalanceMinor != 0 {
		t.Fatalf("wallet_accounts = %s; want only the agent's own GBP account, at zero", text)
	}
}
