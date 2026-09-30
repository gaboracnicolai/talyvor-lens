package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/workspace"
)

// B23.13 — WITH RESERVATIONS OFF, AN AGENT PAYS WHAT ITS QUESTION ACTUALLY COST.
//
// The pre-serve debit is the input-only estimate; after the serve the difference is settled in one row:
// charged up to what the agent's limit still allows, or refunded; what the limit does not allow is written
// off with the question it belongs to. A migrated schema, the real handler, reservations off, agent
// allocation and the shadow debit on. Asserted on lxc_ledger, agent_postings and agent_debit_settlements.

const (
	b2313WS     = "ws-log"
	b2313Key    = "key-b2313"
	b2313Funded = int64(100_000_000) // 100 LXC, cash-backed
	b2313Fund   = int64(10_000_000)  // 10 LXC to the agent
)

// b2313Content is a ~10,000-token prompt; each question varies its last byte so none is a cache hit.
func b2313Content(i int) string { return strings.Repeat("x", 39999) + string(rune('a'+i)) }

func b2313Estimate(t *testing.T) int64 {
	t.Helper()
	_, prompt, _ := extractPrompt([]byte(fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}]}`, b2313Content(0))))
	return lxcEstimate("gpt-4o", prompt)
}

func b2313Setup(t *testing.T, stream bool, promptTokens, completionTokens int, rules *economy.AgentRules) (*Proxy, *economy.DualTokenStore, *pgxpool.Pool, string) {
	t.Helper()
	pool := agentBankDB(t)
	ctx := context.Background()
	store := economy.NewDualTokenStore(nil, pool, nil)
	p, _, _ := newLoggingProxy(t, workspace.LoggingFull)
	p.router = nil // the served model is the requested one, so the delivered cost is known
	p.SetAgentSpender(store, func() bool { return true })
	p.SetReservation(func() bool { return false }, func() int { return 4096 })
	p.SetLXCSpendSink(store, func() bool { return true })
	usage := fmt.Sprintf(`{"prompt_tokens":%d,"completion_tokens":%d}`, promptTokens, completionTokens)
	if stream {
		p.openAIURL = sseUpstream(t, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"+
			"data: {\"choices\":[],\"usage\":"+usage+"}\n\ndata: [DONE]\n\n").URL
	} else {
		p.openAIURL = usageUpstream(t, usage).URL
	}
	seamFund(t, pool, b2313WS, b2313Funded)
	agent, err := store.CreateAgent(ctx, b2313WS, "researcher", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachAgentKey(ctx, b2313WS, agent.ID, b2313Key); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FundAgent(ctx, b2313WS, agent.ID, b2313Fund); err != nil {
		t.Fatal(err)
	}
	if rules != nil {
		if _, err := store.SetAgentRules(ctx, b2313WS, agent.ID, *rules); err != nil {
			t.Fatal(err)
		}
	}
	return p, store, pool, agent.ID
}

func b2313Ask(t *testing.T, p *Proxy, i int, stream bool) {
	t.Helper()
	body := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}],"stream":%t}`, b2313Content(i), stream)
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", b2313WS)
	req.Header.Set("X-Talyvor-Request-ID", fmt.Sprintf("question-%d", i))
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: b2313Key, WorkspaceID: b2313WS}))
	w := newFlushRecorder()
	p.HandleOpenAI(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("question %d: status = %d, want 200; body=%s", i, w.Code, w.Body.String())
	}
}

// b2313Books reads the spend rows and net of lxc_ledger, and what agent_postings says the agent spent and holds.
func b2313Books(t *testing.T, pool *pgxpool.Pool, agentID string) (spendRows, net, spent, holds int64) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE type = 'spend'), COALESCE(sum(amount), 0)::bigint
		FROM lxc_ledger WHERE workspace_id = $1`, b2313WS).Scan(&spendRows, &net); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount_ulxc) FILTER (WHERE account = 'spend'), 0)::bigint,
		COALESCE(sum(amount_ulxc) FILTER (WHERE account = $2), 0)::bigint
		FROM agent_postings WHERE workspace_id = $1`, b2313WS, "agent:"+agentID).Scan(&spent, &holds); err != nil {
		t.Fatal(err)
	}
	return
}

func TestB2313_WithReservationsOff_AnAgentPaysItsDeliveredCost(t *testing.T) {
	estimate := b2313Estimate(t)
	for _, tc := range []struct {
		name                string
		stream              bool
		promptTok, complTok int
	}{
		{"answer cost more than the estimate, buffered", false, 10000, 100},
		{"answer cost more than the estimate, streamed", true, 10000, 100},
		{"answer cost less than the estimate, buffered", false, 5000, 10},
		{"answer cost less than the estimate, streamed", true, 5000, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			delivered := settleULXC(alerts.CostUSD("gpt-4o", tc.promptTok, tc.complTok))
			if delivered == estimate {
				t.Fatalf("delivered %d equals the estimate: the case settles nothing", delivered)
			}
			p, store, pool, agentID := b2313Setup(t, tc.stream, tc.promptTok, tc.complTok, nil)
			b2313Ask(t, p, 0, tc.stream)

			spendRows, net, spent, holds := b2313Books(t, pool, agentID)
			if spendRows != 2 || net != -delivered {
				t.Errorf("lxc_ledger: %d spend rows, net %d µLXC — want the estimate row plus one settling row, net −%d",
					spendRows, net, delivered)
			}
			if spent != delivered || holds != b2313Fund-delivered {
				t.Errorf("agent_postings: spent %d, agent holds %d — want %d spent, %d left", spent, holds, delivered, b2313Fund-delivered)
			}
			if bal := seamBalance(t, store, b2313WS); bal != b2313Funded-delivered {
				t.Errorf("workspace balance %d, want %d", bal, b2313Funded-delivered)
			}
		})
	}
}

func TestB2313_TheLimitCutsAQuestionShort_TheRestIsWrittenOff(t *testing.T) {
	estimate := b2313Estimate(t)
	delivered := settleULXC(alerts.CostUSD("gpt-4o", 10000, 100))
	limit := estimate + 3_000 // the estimate passes; the delivered cost does not
	if delivered <= limit {
		t.Fatalf("delivered %d within the limit %d: nothing to write off", delivered, limit)
	}
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("streamed=%t", stream), func(t *testing.T) {
			p, _, pool, agentID := b2313Setup(t, stream, 10000, 100, &economy.AgentRules{DailyLimitULXC: limit})
			b2313Ask(t, p, 0, stream)

			_, net, spent, _ := b2313Books(t, pool, agentID)
			if spent != limit || net != -limit {
				t.Errorf("agent spent %d, ledger net %d — want exactly its limit %d", spent, net, limit)
			}
			var requestID string
			var writtenOff int64
			if err := pool.QueryRow(context.Background(), `SELECT COALESCE(request_id, ''), written_off_ulxc
				FROM agent_debit_settlements WHERE workspace_id = $1 AND agent_id = $2`, b2313WS, agentID).Scan(&requestID, &writtenOff); err != nil {
				t.Fatal(err)
			}
			if requestID != "question-0" || writtenOff != delivered-limit {
				t.Errorf("settlement: question %q, written off %d — want question-0, %d", requestID, writtenOff, delivered-limit)
			}
		})
	}
}

func TestB2313_TwoConcurrentQuestionsNeverPassTheLimit(t *testing.T) {
	estimate := b2313Estimate(t)
	delivered := settleULXC(alerts.CostUSD("gpt-4o", 10000, 100))
	over := delivered - estimate
	limit := 2*estimate + over // both estimates pass; only one question's difference fits
	p, _, pool, agentID := b2313Setup(t, false, 10000, 100, &economy.AgentRules{DailyLimitULXC: limit})

	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); b2313Ask(t, p, i, false) }()
	}
	wg.Wait()

	_, net, spent, _ := b2313Books(t, pool, agentID)
	if spent != limit || net != -limit {
		t.Errorf("agent spent %d, ledger net %d — want exactly its limit %d, never past it", spent, net, limit)
	}
	var questions, writtenOff int64
	if err := pool.QueryRow(context.Background(), `SELECT count(*), COALESCE(sum(written_off_ulxc), 0)::bigint
		FROM agent_debit_settlements WHERE workspace_id = $1`, b2313WS).Scan(&questions, &writtenOff); err != nil {
		t.Fatal(err)
	}
	if questions != 2 || writtenOff != 2*delivered-limit {
		t.Errorf("%d questions settled, %d written off — want 2, %d", questions, writtenOff, 2*delivered-limit)
	}
}
