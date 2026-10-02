package proxy

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/localrouter"
	"github.com/talyvor/lens/internal/workspace"
)

// B26.5 — AN AGENT QUESTION SERVED BY A NODE OR BY LOCAL ROUTING SETTLES ITS HOLD.
//
// Both paths answered the question and returned without settling or releasing the agent's hold, so the
// stranded sweeper later refunded it in full and the question was free. Each must now end with exactly one
// settled debit equal to its cost. A migrated schema, the real handler, an agent with its own balance, and
// an upstream that fails the test if it is called. Asserted on lxc_ledger and agent_postings.
func TestB265_OffSeamServeSettlesTheAgentsHold(t *testing.T) {
	const (
		ws, key  = "default", "key-b265" // local routing serves the default workspace only
		funded   = int64(100_000_000)    // 100 LXC, cash-backed
		fund     = int64(10_000_000)     // 10 LXC to the agent
		prompt   = "what is two plus two?"
		nodeText = "two plus two is four, as the node computed"
	)

	for _, tc := range []struct {
		name   string
		wire   func(t *testing.T, p *Proxy)
		header string
		// cost is the delivered cost in µLXC, from the response body the agent received.
		cost func(body []byte) int64
	}{
		{
			name: "served by a node",
			wire: func(t *testing.T, p *Proxy) {
				node := fakeInferenceNode(t, nodeText, 3, 9)
				r := localrouter.NewRouter(nil)
				r.Register(&localrouter.LocalEndpoint{ID: "node-1", URL: node.URL, Provider: "vllm", Models: []string{"gpt-4o"}, Healthy: true})
				r.UpdateQuality("node-1", "gpt-4o", 0.9, 10) // an agent routes price-aware, which wants a qualified node
				p.SetNodeRouter(r, ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)), node.Client(), true)
			},
			header: "X-Talyvor-Node-Served",
			cost: func([]byte) int64 {
				usd, _ := alerts.CostUSDResolved("gpt-4o", catalog.PurposeCharge, len(prompt)/4, 0, 0, len(nodeText)/4)
				return settleULXC(usd)
			},
		},
		{
			name:   "served by local routing",
			wire:   func(t *testing.T, p *Proxy) { p.localRouter = fakeOllama(t) },
			header: "X-Talyvor-Local-Model",
			cost: func(body []byte) int64 {
				usd, _ := alerts.CostUSDResolved("llama3.2:latest", catalog.PurposeCharge, len(prompt)/4, 0, 0, len(body)/4)
				return settleULXC(usd)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := agentBankDB(t)
			ctx := context.Background()
			store := economy.NewDualTokenStore(nil, pool, nil)
			p, _, _ := newLoggingProxy(t, workspace.LoggingFull)
			if err := p.workspaceManager.RegisterWorkspace(ctx, workspace.Workspace{
				ID: ws, Name: ws, Active: true, LoggingPolicy: workspace.LoggingFull,
			}); err != nil {
				t.Fatal(err)
			}
			p.router = nil
			p.SetAgentSpender(store, func() bool { return true })
			p.SetReservation(func() bool { return true }, func() int { return 4096 })
			p.SetLXCSpendSink(store, func() bool { return true })
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				t.Error("the upstream was called — the question was not served off the upstream seams")
				_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"upstream"}}]}`)
			}))
			t.Cleanup(upstream.Close)
			p.openAIURL = upstream.URL
			tc.wire(t, p)

			seamFund(t, pool, ws, funded)
			agent, err := store.CreateAgent(ctx, ws, "researcher", "user-owner")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.AttachAgentKey(ctx, ws, agent.ID, key); err != nil {
				t.Fatal(err)
			}
			if _, err := store.FundAgent(ctx, ws, agent.ID, fund); err != nil {
				t.Fatal(err)
			}

			body, _ := json.Marshal(map[string]any{"model": "gpt-4o", "messages": []map[string]string{{"role": "user", "content": prompt}}})
			req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Talyvor-Workspace", ws)
			req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: key, WorkspaceID: ws}))
			w := httptest.NewRecorder()
			p.HandleOpenAI(w, req)
			if w.Code != http.StatusOK || w.Header().Get(tc.header) == "" {
				t.Fatalf("status %d, %s %q — want 200 from the off-seam path; body=%s", w.Code, tc.header, w.Header().Get(tc.header), w.Body.String())
			}
			want := tc.cost(w.Body.Bytes())
			if want <= 0 {
				t.Fatalf("precondition: the delivered cost is %d µLXC — it must be positive to tell a settle from a refund", want)
			}

			var status string
			var settled int64
			if err := pool.QueryRow(ctx, `SELECT status, COALESCE(settled_ulxc, -1) FROM lxc_reservations WHERE workspace_id = $1`, ws).
				Scan(&status, &settled); err != nil {
				t.Fatalf("exactly one hold for the question: %v", err)
			}
			if status != "settled" || settled != want {
				t.Errorf("hold is %s at %d µLXC — want settled at %d (a held one is swept and refunded: the question is free)", status, settled, want)
			}
			var spends, spent, net int64
			if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE type = 'spend'),
				COALESCE(-sum(amount) FILTER (WHERE type = 'spend'), 0)::bigint, COALESCE(sum(amount), 0)::bigint
				FROM lxc_ledger WHERE workspace_id = $1`, ws).Scan(&spends, &spent, &net); err != nil {
				t.Fatal(err)
			}
			if spends != 1 || spent != want || net != -want {
				t.Errorf("lxc_ledger: %d spend rows totalling %d µLXC, net %d — want 1 totalling %d, net −%d", spends, spent, net, want, want)
			}
			var agentBal, spendAcct int64
			if err := pool.QueryRow(ctx, `SELECT
				COALESCE(sum(amount_ulxc) FILTER (WHERE account = $2), 0)::bigint,
				COALESCE(sum(amount_ulxc) FILTER (WHERE account = 'spend'), 0)::bigint
				FROM agent_postings WHERE workspace_id = $1`, ws, "agent:"+agent.ID).Scan(&agentBal, &spendAcct); err != nil {
				t.Fatal(err)
			}
			if spendAcct != want || agentBal != fund-want {
				t.Errorf("agent_postings: spend account %d, agent holds %d — want %d spent, %d left", spendAcct, agentBal, want, fund-want)
			}
		})
	}
}
