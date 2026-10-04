package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B28.298 — through the routes the suite calls, on a migrated schema: an agent is renamed and described,
// then archived with a request of its key still in flight. Archiving writes ONE withdraw entry of the agent's
// whole balance and revokes its key; the in-flight request still settles, and the key then writes no hold —
// even though the settle's refund left the agent a balance that would cover it.
func TestAgentRoutes_ArchivingSweepsTheBalanceInOneWithdrawAndTheKeyThenWritesNoHold(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-lifecycle"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 20000000, 20000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	keys := tenant.NewStore(pool)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, keys)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	base := "/v1/workspaces/" + ws + "/agents"

	_, a := call(http.MethodPost, base, `{"name":"researcher"}`)
	agent := a["id"].(string)
	_, k := call(http.MethodPost, base+"/"+agent+"/keys", `{"name":"researcher key"}`)
	rawKey, keyID := k["key"].(string), k["id"].(string)
	if code, out := call(http.MethodPost, base+"/"+agent+"/fund", `{"amount_ulxc":7500000}`); code != http.StatusOK {
		t.Fatalf("fund = %d %v", code, out)
	}

	// Rename and describe.
	code, out := call(http.MethodPatch, base+"/"+agent, `{"name":"  market researcher ","description":"reads filings, summarises them"}`)
	if code != http.StatusOK || out["name"] != "market researcher" || out["description"] != "reads filings, summarises them" ||
		out["balance_ulxc"].(float64) != 7_500_000 {
		t.Fatalf("patch = %d %v", code, out)
	}
	if code, out := call(http.MethodPatch, base+"/"+agent, `{"name":"   "}`); code != http.StatusBadRequest {
		t.Fatalf("a blank name = %d %v, want 400", code, out)
	}

	// A request of the agent's key is in flight when it is archived: 1 LXC held of its 7.5.
	if err := store.ReserveLXCForAgent(ctx, keyID, ws, "res-in-flight", 1_000_000, economy.AgentDebitMeta{}); err != nil {
		t.Fatalf("the key could not hold before archiving: %v", err)
	}

	code, out = call(http.MethodPost, base+"/"+agent+"/archive", ``)
	if code != http.StatusOK || out["swept_ulxc"].(float64) != 6_500_000 {
		t.Fatalf("archive = %d %v, want 6500000 swept", code, out)
	}
	if revoked := out["revoked_keys"].([]any); len(revoked) != 1 || revoked[0] != keyID {
		t.Fatalf("revoked %v, want [%s]", revoked, keyID)
	}

	// The ledger: one withdraw entry, the agent's whole balance to the workspace.
	type posting struct {
		account string
		amount  int64
	}
	rows, err := pool.Query(ctx, `SELECT account, amount_ulxc FROM agent_postings WHERE workspace_id = $1 AND kind = 'withdraw' ORDER BY amount_ulxc`, ws)
	if err != nil {
		t.Fatal(err)
	}
	var withdraws []posting
	for rows.Next() {
		var p posting
		if err := rows.Scan(&p.account, &p.amount); err != nil {
			t.Fatal(err)
		}
		withdraws = append(withdraws, p)
	}
	if want := []posting{{"agent:" + agent, -6_500_000}, {"workspace", 6_500_000}}; len(withdraws) != 2 || withdraws[0] != want[0] || withdraws[1] != want[1] {
		t.Fatalf("withdraw postings %v, want %v", withdraws, want)
	}

	// The key is revoked: it no longer authenticates.
	if _, err := keys.ValidateAPIKey(ctx, rawKey); err == nil {
		t.Fatal("the archived agent's key still authenticates")
	}

	// The request in flight still settles at 0.4 LXC, refunding 0.6 to the agent.
	if _, _, err := store.SettleLXCReservation(ctx, "res-in-flight", 400_000, economy.AgentDebitMeta{}); err != nil {
		t.Fatalf("the in-flight request did not settle: %v", err)
	}
	// The key then writes no hold, though the refund would cover one.
	err = store.ReserveLXCForAgent(ctx, keyID, ws, "res-after", 100_000, economy.AgentDebitMeta{})
	if !errors.Is(err, economy.ErrAgentRule) {
		t.Fatalf("a hold after archiving = %v, want refused as archived", err)
	}
	var holds int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_postings WHERE workspace_id = $1 AND ref = 'res-after'`, ws).Scan(&holds); err != nil {
		t.Fatal(err)
	}
	if holds != 0 {
		t.Fatalf("the archived agent's key wrote %d hold postings, want 0", holds)
	}

	// It cannot be funded, given a key, or archived again; what the refund left is still the owner's to withdraw.
	if code, out := call(http.MethodPost, base+"/"+agent+"/fund", `{"amount_ulxc":1}`); code != http.StatusConflict {
		t.Fatalf("fund an archived agent = %d %v, want 409", code, out)
	}
	if code, out := call(http.MethodPost, base+"/"+agent+"/keys", `{}`); code != http.StatusConflict {
		t.Fatalf("a key for an archived agent = %d %v, want 409", code, out)
	}
	if code, out := call(http.MethodPost, base+"/"+agent+"/archive", ``); code != http.StatusConflict {
		t.Fatalf("archive again = %d %v, want 409", code, out)
	}
	if code, out := call(http.MethodPost, base+"/"+agent+"/withdraw", `{"amount_ulxc":600000}`); code != http.StatusOK || out["balance_ulxc"].(float64) != 0 {
		t.Fatalf("withdraw the refund = %d %v", code, out)
	}

	// The list shows it archived, with its name and description.
	_, book := call(http.MethodGet, base, ``)
	got := book["agents"].([]any)[0].(map[string]any)
	if got["archived_at"] == nil || got["name"] != "market researcher" || got["description"] != "reads filings, summarises them" {
		t.Fatalf("listed as %v", got)
	}
}
