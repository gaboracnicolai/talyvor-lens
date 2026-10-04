package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/workspace"
)

// B19.2 — EACH OF AN AGENT'S SPENDING RULES REFUSES THE REQUEST THAT BREAKS IT BEFORE THE PROVIDER IS
// CALLED, BUFFERED AND STREAMED, AND A RETRIED REQUEST IS CHARGED ONCE.
//
// The real handler with the agent reservation on, a migrated schema, an upstream that counts its calls.
// Every request asks for a fresh answer (X-Talyvor-Cache: bypass), so every question let through reaches
// the upstream and is charged, and a repeat is not quietly a free cache hit.
func TestB192_EachAgentRuleRefusesBeforeTheProvider_AndARetryIsChargedOnce(t *testing.T) {
	pool := agentBankDB(t)
	ctx := context.Background()
	store := economy.NewDualTokenStore(nil, pool, nil)
	p, _, _ := newLoggingProxy(t, workspace.LoggingFull)
	p.router = nil
	p.SetAgentSpender(store, func() bool { return true })
	p.SetReservation(func() bool { return true }, func() int { return 4096 })
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10000,"completion_tokens":100}}`)
	}))
	t.Cleanup(upstream.Close)
	p.openAIURL = upstream.URL

	const ws = "ws-log"
	seamFund(t, pool, ws, 100_000_000)
	agent, err := store.CreateAgent(ctx, ws, "researcher", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachAgentKey(ctx, ws, agent.ID, "key-agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FundAgent(ctx, ws, agent.ID, 50_000_000); err != nil {
		t.Fatal(err)
	}

	type answer struct {
		code int
		body string
	}
	ask := func(model string, stream bool, idempotencyKey string) answer {
		t.Helper()
		body := fmt.Sprintf(`{"model":%q,"stream":%t,"messages":[{"role":"user","content":%q}]}`, model, stream, strings.Repeat("q", 40000))
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Talyvor-Workspace", ws)
		req.Header.Set(CacheBypassHeader, "bypass")
		if idempotencyKey != "" {
			req.Header.Set("Idempotency-Key", idempotencyKey)
		}
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: "key-agent", WorkspaceID: ws}))
		w := httptest.NewRecorder()
		p.HandleOpenAI(w, req)
		return answer{w.Code, w.Body.String()}
	}
	agentBalance := func() int64 {
		t.Helper()
		book, err := store.AgentBook(ctx, ws)
		if err != nil {
			t.Fatal(err)
		}
		return book.Agents[0].BalanceULXC
	}
	setRules := func(r economy.AgentRules) {
		t.Helper()
		var none int64 // each case's rules replace the last case's whole, hourly and weekly caps too
		if r.HourlyLimitULXC == nil {
			r.HourlyLimitULXC = &none
		}
		if r.WeeklyLimitULXC == nil {
			r.WeeklyLimitULXC = &none
		}
		if _, err := store.SetAgentRules(ctx, ws, agent.ID, r); err != nil {
			t.Fatalf("set rules %+v: %v", r, err)
		}
	}

	// One question with no rules: what a question costs, and what it holds.
	if a := ask("gpt-4o", false, ""); a.code != http.StatusOK {
		t.Fatalf("a question with no rules = %d %s", a.code, a.body)
	}
	charge := 50_000_000 - agentBalance()
	hold := reserveEstimateLXC("gpt-4o", strings.Repeat("q", 40000), 4096)
	if charge <= 0 || hold <= charge {
		t.Fatalf("a question charged %d µLXC and holds %d — want a positive charge below the hold", charge, hold)
	}

	capped := charge + hold - 1
	now := time.Now().UTC()
	elsewhen := func(h int) string { return now.Add(time.Duration(h) * time.Hour).Format("15:04") }
	for _, tc := range []struct {
		rule  string
		rules economy.AgentRules
		model string
		says  string
	}{
		{"limit per request", economy.AgentRules{MaxPerRequestULXC: hold - 1}, "gpt-4o", "limit per request"},
		{"daily limit", economy.AgentRules{DailyLimitULXC: charge + hold - 1}, "gpt-4o", "daily limit"},
		{"monthly limit", economy.AgentRules{MonthlyLimitULXC: charge + hold - 1}, "gpt-4o", "monthly limit"},
		{"hourly limit", economy.AgentRules{HourlyLimitULXC: &capped}, "gpt-4o", "hourly limit"},
		{"weekly limit", economy.AgentRules{WeeklyLimitULXC: &capped}, "gpt-4o", "weekly limit"},
		{"allowed models", economy.AgentRules{AllowedModels: []string{"gpt-4o-mini"}}, "gpt-4o", `model \"gpt-4o\"`},
		{"allowed providers", economy.AgentRules{AllowedProviders: []string{"anthropic"}}, "gpt-4o", `provider \"openai\"`},
		{"active hours", economy.AgentRules{ActiveFrom: elsewhen(2), ActiveUntil: elsewhen(3)}, "gpt-4o", "may spend only between"},
		{"approval amount", economy.AgentRules{ApprovalAboveULXC: hold - 1}, "gpt-4o", "must be approved"},
	} {
		setRules(tc.rules)
		for _, stream := range []bool{false, true} {
			calls, balance := upstreamCalls.Load(), agentBalance()
			a := ask(tc.model, stream, "")
			if a.code != http.StatusForbidden || !strings.Contains(a.body, tc.says) {
				t.Errorf("%s (stream=%t): %d %s — want 403 saying %q", tc.rule, stream, a.code, a.body, tc.says)
			}
			if upstreamCalls.Load() != calls || agentBalance() != balance {
				t.Errorf("%s (stream=%t): the provider was called or the agent charged on a refused request", tc.rule, stream)
			}
		}
	}

	// The approval: the owner approves the pending request, it goes through once, and a repeat needs
	// approving again.
	approvals, err := store.ListAgentApprovals(ctx, ws)
	if err != nil || len(approvals) != 1 || approvals[0].Status != "pending" || approvals[0].AmountULXC != hold {
		t.Fatalf("approvals = %+v, %v — want one pending approval of %d µLXC for both refusals", approvals, err, hold)
	}
	if _, err := store.DecideAgentApproval(ctx, ws, approvals[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if a := ask("gpt-4o", false, ""); a.code != http.StatusOK {
		t.Errorf("the approved request = %d %s, want 200", a.code, a.body)
	}
	if a := ask("gpt-4o", false, ""); a.code != http.StatusForbidden {
		t.Errorf("the approved request asked a second time = %d, want 403 — an approval lets it through once", a.code)
	}

	// A retry repeating its Idempotency-Key is served and charged once; a new key is a new charge.
	setRules(economy.AgentRules{})
	balance, calls := agentBalance(), upstreamCalls.Load()
	for i := 0; i < 2; i++ {
		if a := ask("gpt-4o", false, "retry-7"); a.code != http.StatusOK {
			t.Fatalf("try %d with Idempotency-Key retry-7 = %d %s", i+1, a.code, a.body)
		}
	}
	if got := balance - agentBalance(); got != charge {
		t.Errorf("a request and its retry charged %d µLXC, want one charge of %d", got, charge)
	}
	if upstreamCalls.Load() != calls+2 {
		t.Errorf("the provider saw %d calls for the request and its retry, want 2", upstreamCalls.Load()-calls)
	}
	if a := ask("gpt-4o", false, "retry-8"); a.code != http.StatusOK || balance-agentBalance() != 2*charge {
		t.Errorf("a new Idempotency-Key = %d and %d µLXC charged in all, want 200 and %d", a.code, balance-agentBalance(), 2*charge)
	}
	// The ledger agrees: one delivered-charge row per question charged — the first, the approved one,
	// retry-7 once, retry-8 — each of the same amount, and nothing else ever charged.
	var rows, total int64
	if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(-sum(amount), 0)::bigint FROM lxc_ledger
		WHERE workspace_id = $1 AND type = 'spend'`, ws).Scan(&rows, &total); err != nil {
		t.Fatal(err)
	}
	if rows != 4 || total != 4*charge {
		t.Errorf("lxc_ledger has %d spend rows totalling %d µLXC, want 4 totalling %d", rows, total, 4*charge)
	}
}
