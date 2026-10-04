package proxy

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
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

// hangUpRecorder is a client that goes away the moment its answer arrives: the first Write cancels the
// request's context, as net/http does when the connection drops after the response is flushed.
type hangUpRecorder struct {
	*httptest.ResponseRecorder
	hangUp context.CancelFunc
}

func (h *hangUpRecorder) Write(b []byte) (int, error) {
	n, err := h.ResponseRecorder.Write(b)
	h.hangUp()
	return n, err
}

// B27.4 — A CLIENT THAT DISCONNECTS RIGHT AFTER THE ANSWER STILL PAYS FOR IT.
//
// The buffered, node and local settles ran on the request's context, so a client gone right after the
// response cancelled the settle and left the hold for the stranded sweeper to refund: the question was free.
// A migrated schema, the real handler, an agent with its own balance and reservations on. Asserted on
// lxc_reservations and lxc_ledger.
func TestB274_AClientThatHangsUpAfterTheAnswerStillPays(t *testing.T) {
	const (
		ws, key  = "default", "key-b274" // local routing serves the default workspace only
		funded   = int64(100_000_000)    // 100 LXC, cash-backed
		fund     = int64(10_000_000)     // 10 LXC to the agent
		prompt   = "what is two plus two?"
		nodeText = "two plus two is four, as the node computed"
	)

	for _, tc := range []struct {
		name string
		wire func(t *testing.T, p *Proxy)
		// cost is the delivered cost in µLXC, from the response body the agent received.
		cost func(body []byte) int64
	}{
		{
			name: "buffered upstream answer",
			wire: func(t *testing.T, p *Proxy) {
				p.openAIURL = usageUpstream(t, `{"prompt_tokens":100,"completion_tokens":10}`).URL
			},
			cost: func([]byte) int64 {
				usd, _ := alerts.CostUSDResolved("gpt-4o", catalog.PurposeCharge, 100, 0, 0, 10)
				return settleULXC(usd)
			},
		},
		{
			name: "served by a node",
			wire: func(t *testing.T, p *Proxy) {
				node := fakeInferenceNode(t, nodeText, 3, 9)
				r := localrouter.NewRouter(nil)
				r.Register(&localrouter.LocalEndpoint{ID: "node-1", URL: node.URL, Provider: "vllm", Models: []string{"gpt-4o"}, Healthy: true})
				r.UpdateQuality("node-1", "gpt-4o", 0.9, 10)
				p.SetNodeRouter(r, ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)), node.Client(), true)
			},
			cost: func([]byte) int64 {
				usd, _ := alerts.CostUSDResolved("gpt-4o", catalog.PurposeCharge, len(prompt)/4, 0, 0, len(nodeText)/4)
				return settleULXC(usd)
			},
		},
		{
			name: "served by local routing",
			wire: func(t *testing.T, p *Proxy) { p.localRouter = fakeOllama(t) },
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
			clientCtx, hangUp := context.WithCancel(context.Background())
			t.Cleanup(hangUp)
			req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Talyvor-Workspace", ws)
			req = req.WithContext(auth.WithAuthContext(clientCtx, &auth.AuthContext{APIKeyID: key, WorkspaceID: ws}))
			w := &hangUpRecorder{ResponseRecorder: httptest.NewRecorder(), hangUp: hangUp}
			p.HandleOpenAI(w, req)
			if w.Code != http.StatusOK || clientCtx.Err() == nil {
				t.Fatalf("status %d, client gone = %v — want a 200 answer the client hung up on; body=%s",
					w.Code, clientCtx.Err() != nil, w.Body.String())
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
			var spends, spent int64
			if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE type = 'spend'),
				COALESCE(-sum(amount) FILTER (WHERE type = 'spend'), 0)::bigint
				FROM lxc_ledger WHERE workspace_id = $1`, ws).Scan(&spends, &spent); err != nil {
				t.Fatal(err)
			}
			if spends != 1 || spent != want {
				t.Errorf("lxc_ledger: %d spend rows totalling %d µLXC — want 1 totalling %d", spends, spent, want)
			}
		})
	}
}
