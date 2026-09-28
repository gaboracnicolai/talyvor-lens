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
)

// B19.9 — an agent, through the MCP server with its own key, checks its balance, asks for approval,
// pays once approved, and is refused a payment that breaks its rules; every call is logged.
func TestAgentTools_AnAgentUsesTheBankThroughMCP(t *testing.T) {
	pool := savingsTestPool(t)
	ctx := context.Background()
	const ws = "ws-agent-tools"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 20000000, 20000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	buyer, err := store.CreateAgent(ctx, ws, "buyer")
	if err != nil {
		t.Fatal(err)
	}
	seller, err := store.CreateAgent(ctx, ws, "seller")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachAgentKey(ctx, ws, buyer.ID, "key-buyer"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FundAgent(ctx, ws, buyer.ID, 10_000_000); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAgentRules(ctx, ws, buyer.ID, economy.AgentRules{ApprovalAboveULXC: 1_000_000, MaxPerRequestULXC: 5_000_000}); err != nil {
		t.Fatal(err)
	}
	srv := newServer(pool, nil, nil, nil, nil, "test")
	srv.SetAgentBank(store)

	rpc := func(key, method, params string) map[string]any {
		t.Helper()
		body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":` + params + `}`
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: key, WorkspaceID: ws}))
		w := httptest.NewRecorder()
		srv.HandleRPC(w, req)
		var resp map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp["error"] != nil {
			t.Fatalf("%s %s = %s", method, params, w.Body.String())
		}
		return resp["result"].(map[string]any)
	}
	// call returns the tool's text and whether the bank refused it.
	call := func(key, tool, args string) (string, bool) {
		t.Helper()
		res := rpc(key, "tools/call", `{"name":"`+tool+`","arguments":`+args+`}`)
		refused, _ := res["isError"].(bool)
		return res["content"].([]any)[0].(map[string]any)["text"].(string), refused
	}
	balance := func() int64 {
		t.Helper()
		text, refused := call("key-buyer", "agent_balance", `{}`)
		var out struct {
			Agent economy.Agent      `json:"agent"`
			Rules economy.AgentRules `json:"rules"`
		}
		if refused || json.Unmarshal([]byte(text), &out) != nil || out.Agent.ID != buyer.ID || out.Rules.ApprovalAboveULXC != 1_000_000 {
			t.Fatalf("agent_balance = %s", text)
		}
		return out.Agent.BalanceULXC
	}

	listed := map[string]bool{}
	for _, tool := range rpc("key-buyer", "tools/list", `{}`)["tools"].([]any) {
		listed[tool.(map[string]any)["name"].(string)] = true
	}
	for _, name := range []string{"agent_balance", "agent_request_approval", "agent_pay", "agent_receipt"} {
		if !listed[name] {
			t.Errorf("tools/list does not offer %s", name)
		}
	}

	if b := balance(); b != 10_000_000 {
		t.Fatalf("the agent sees a balance of %d, want 10,000,000", b)
	}
	payment := `{"to_agent_id":"` + seller.ID + `","amount_ulxc":2000000,"memo":"invoice 7"}`
	text, refused := call("key-buyer", "agent_request_approval", `{"to_agent_id":"`+seller.ID+`","amount_ulxc":2000000,"memo":"invoice 7","reason":"October hosting"}`)
	var approval economy.AgentApproval
	if refused || json.Unmarshal([]byte(text), &approval) != nil || approval.Status != "pending" || approval.Reason != "October hosting" {
		t.Fatalf("agent_request_approval = %s", text)
	}
	if text, refused := call("key-buyer", "agent_pay", payment); !refused || !strings.Contains(text, approval.ID) {
		t.Errorf("paying before the approval = %s, want refused naming %s", text, approval.ID)
	}
	if _, err := store.DecideAgentApproval(ctx, ws, approval.ID, true); err != nil {
		t.Fatal(err)
	}
	text, refused = call("key-buyer", "agent_pay", payment)
	var pay economy.AgentPayment
	if refused || json.Unmarshal([]byte(text), &pay) != nil || pay.FromBalanceULXC != 8_000_000 {
		t.Fatalf("paying once approved = %s", text)
	}
	if text, refused := call("key-buyer", "agent_pay", payment); !refused || !strings.Contains(text, "approv") {
		t.Errorf("paying a second time on one approval = %s, want refused", text)
	}
	// Beyond its limit per request: refused, and nothing moves.
	if text, refused := call("key-buyer", "agent_pay", `{"to_agent_id":"`+seller.ID+`","amount_ulxc":6000000}`); !refused || !strings.Contains(text, "limit per request") {
		t.Errorf("a payment beyond the limit per request = %s, want refused", text)
	}
	if b := balance(); b != 8_000_000 {
		t.Errorf("the agent holds %d after one approved 2 LXC payment, want 8,000,000", b)
	}
	text, refused = call("key-buyer", "agent_receipt", `{"entry_id":"`+pay.EntryID+`"}`)
	var receipt economy.AgentReceipt
	if refused || json.Unmarshal([]byte(text), &receipt) != nil || receipt.Kind != "pay" || len(receipt.Postings) != 2 ||
		receipt.Postings[0].AmountULXC+receipt.Postings[1].AmountULXC != 0 {
		t.Errorf("agent_receipt = %s, want the payment's two postings", text)
	}
	// A key attached to no agent reaches no account.
	if text, refused := call("key-nobody", "agent_balance", `{}`); !refused || !strings.Contains(text, "not attached to an agent") {
		t.Errorf("a key attached to no agent = %s, want refused", text)
	}

	// Every call is in the log, in order, with its outcome.
	rows, err := pool.Query(ctx, `SELECT tool, outcome, agent_id, scoped_key_id FROM agent_tool_calls ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var tool, outcome, agent, key string
		if err := rows.Scan(&tool, &outcome, &agent, &key); err != nil {
			t.Fatal(err)
		}
		if (key == "key-buyer") != (agent == buyer.ID) {
			t.Errorf("logged %s by %s as agent %q", tool, key, agent)
		}
		got = append(got, tool+" "+outcome)
	}
	rows.Close()
	want := []string{"agent_balance ok", "agent_request_approval ok", "agent_pay refused", "agent_pay ok", "agent_pay refused",
		"agent_pay refused", "agent_balance ok", "agent_receipt ok", "agent_balance refused"}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("the log has\n  %s\nwant\n  %s", strings.Join(got, ", "), strings.Join(want, ", "))
	}
}
