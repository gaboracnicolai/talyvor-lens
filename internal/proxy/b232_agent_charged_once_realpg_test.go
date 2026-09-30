package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/workspace"
)

// B23.2 — AN AGENT'S QUESTION IS CHARGED EXACTLY ONCE.
//
// Two configurations charged an agent's question a number of times other than one:
//   - request logging "none": the settle sat inside the logging gate, so the hold was never settled and the
//     stranded sweeper refunded it in full — the question was FREE;
//   - reservations off with the shadow debit on: the pre-serve agent debit, then the post-serve shadow
//     SpendLXC of the same question — charged TWICE.
//
// A migrated schema, the real handler, an agent with its own balance, buffered and streamed. Asserted on
// lxc_ledger and agent_postings, never a status code.
func TestB232_AnAgentsQuestionIsChargedExactlyOnce(t *testing.T) {
	const (
		ws, key = "ws-log", "key-b232"
		funded  = int64(100_000_000) // 100 LXC, cash-backed
		fund    = int64(10_000_000)  // 10 LXC to the agent
	)
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10000,\"completion_tokens\":100}}\n\n" +
		"data: [DONE]\n\n"
	content := strings.Repeat("x", 40000)
	delivered := settleULXC(alerts.CostUSD("gpt-4o", 10000, 100))

	for _, tc := range []struct {
		name        string
		logging     workspace.LoggingPolicy
		reservation bool
		stream      bool
	}{
		{"logging none, buffered", workspace.LoggingNone, true, false},
		{"logging none, streamed", workspace.LoggingNone, true, true},
		{"reservations off, buffered", workspace.LoggingFull, false, false},
		{"reservations off, streamed", workspace.LoggingFull, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := agentBankDB(t)
			ctx := context.Background()
			store := economy.NewDualTokenStore(nil, pool, nil)
			p, _, _ := newLoggingProxy(t, tc.logging)
			p.router = nil // the served model is the requested one, so the delivered cost is known
			p.SetAgentSpender(store, func() bool { return true })
			p.SetReservation(func() bool { return tc.reservation }, func() int { return 4096 })
			p.SetLXCSpendSink(store, func() bool { return true })
			if tc.stream {
				p.openAIURL = sseUpstream(t, sse).URL
			} else {
				p.openAIURL = usageUpstream(t, `{"prompt_tokens":10000,"completion_tokens":100}`).URL
			}
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

			body := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}],"stream":%t}`, content, tc.stream)
			req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Talyvor-Workspace", ws)
			req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: key, WorkspaceID: ws}))
			w := newFlushRecorder()
			p.HandleOpenAI(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
			}

			// The charge is the delivered cost in both. With reservations off it is the pre-serve estimate plus
			// the one row settling it to the delivered cost (B23.13) — still no shadow debit on top.
			want, wantRows := delivered, int64(1)
			if !tc.reservation {
				wantRows = 2
			}

			var spends, spent, net int64
			if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE type = 'spend'),
				COALESCE(-sum(amount) FILTER (WHERE type = 'spend'), 0)::bigint, COALESCE(sum(amount), 0)::bigint
				FROM lxc_ledger WHERE workspace_id = $1`, ws).Scan(&spends, &spent, &net); err != nil {
				t.Fatal(err)
			}
			if spends != wantRows || spent != want || net != -want {
				t.Errorf("lxc_ledger: %d spend rows totalling %d µLXC, net %d — want %d totalling %d, net −%d",
					spends, spent, net, wantRows, want, want)
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
			if bal := seamBalance(t, store, ws); bal != funded-want {
				t.Errorf("workspace balance %d, want %d", bal, funded-want)
			}
		})
	}
}
