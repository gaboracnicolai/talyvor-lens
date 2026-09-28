package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/workspace"
)

// B19.13 — A WORKSPACE'S OWN SPENDING CANNOT USE THE LXC ITS AGENTS HOLD.
//
// 10 LXC in the workspace, 8 of them funded to an agent. Through the real handler, a question whose
// hold is 3 LXC is refused (402) on the workspace's own key, buffered and streamed, and served on the
// agent's key. The workspace's direct spend (browser chat, usage beyond a plan) is refused past its 2
// unallocated LXC too, and nothing the refusals touched moved.
func TestB1913_TheWorkspacesOwnSpendIsHeldToItsUnallocatedLXC(t *testing.T) {
	pool := agentBankDB(t)
	ctx := context.Background()
	store := economy.NewDualTokenStore(nil, pool, nil)
	p, _, _ := newLoggingProxy(t, workspace.LoggingFull)
	p.router = nil
	p.SetAgentSpender(store, func() bool { return true })
	const maxOut = 30_000 // gpt-4o output at $10 per million: a 30,000-token allowance holds 3 LXC
	p.SetReservation(func() bool { return true }, func() int { return maxOut })
	p.openAIURL = usageUpstream(t, `{"prompt_tokens":10000,"completion_tokens":100}`).URL

	const ws = "ws-log"
	seamFund(t, pool, ws, 10_000_000) // 10 LXC
	agent, err := store.CreateAgent(ctx, ws, "researcher")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachAgentKey(ctx, ws, agent.ID, "key-agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FundAgent(ctx, ws, agent.ID, 8_000_000); err != nil { // 8 LXC held by the agent
		t.Fatal(err)
	}

	const prompt = "what does the ledger say about this quarter?"
	hold := reserveEstimateLXC("gpt-4o", prompt, maxOut)
	if hold <= 2_000_000 || hold > 8_000_000 {
		t.Fatalf("the question holds %d µLXC — it must cost more than the workspace's 2 unallocated LXC and no more than the agent's 8", hold)
	}
	t.Logf("the question holds %d µLXC", hold)
	ask := func(key string, stream bool) int {
		t.Helper()
		body := fmt.Sprintf(`{"model":"gpt-4o","stream":%t,"messages":[{"role":"user","content":%q}]}`, stream, prompt)
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Talyvor-Workspace", ws)
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: key, WorkspaceID: ws}))
		w := httptest.NewRecorder()
		p.HandleOpenAI(w, req)
		return w.Code
	}

	if code := ask("key-workspace", false); code != http.StatusPaymentRequired {
		t.Errorf("the workspace's own question = %d, want 402 — it spent LXC its agent holds", code)
	}
	if code := ask("key-workspace", true); code != http.StatusPaymentRequired {
		t.Errorf("the workspace's own streamed question = %d, want 402 — it spent LXC its agent holds", code)
	}
	if err := store.SpendLXC(ctx, ws, 3_000_000, "chat: metered usage"); !errors.Is(err, economy.ErrInsufficientLXC) {
		t.Errorf("a 3 LXC direct spend by the workspace = %v, want ErrInsufficientLXC", err)
	}
	if code := ask("key-agent", false); code != http.StatusOK {
		t.Errorf("the agent's question = %d, want 200 — it holds 8 LXC", code)
	}

	book, err := store.AgentBook(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	charge := 8_000_000 - book.Agents[0].BalanceULXC
	if charge <= 0 || book.WorkspaceBalanceULXC != 10_000_000-charge || book.UnallocatedULXC != 2_000_000 {
		t.Errorf("book = %+v: want the agent charged (%d µLXC) from its own 8 LXC, the workspace down by exactly that, and 2 LXC still unallocated",
			book, charge)
	}
	var ledgerNet int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount), 0)::bigint FROM lxc_ledger WHERE workspace_id = $1`, ws).Scan(&ledgerNet); err != nil {
		t.Fatal(err)
	}
	if ledgerNet != -charge {
		t.Errorf("lxc_ledger nets %d µLXC, want −%d: only the agent's question may have moved money", ledgerNet, charge)
	}
	if got, err := store.GetUnallocatedLXC(ctx, ws); err != nil || got != 2_000_000 {
		t.Errorf("GetUnallocatedLXC = %d, %v; want 2,000,000", got, err)
	}
}
