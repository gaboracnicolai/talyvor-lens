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

	"github.com/google/uuid"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/workspace"
)

// B19.6 — AN AGENT SPENDING FIVE TIMES ITS USUAL RATE RAISES ONE ALERT AND, WITH AUTO-PAUSE ON, IS PAUSED
// BEFORE ITS NEXT REQUEST.
//
// The real handler with the agent reservation on, a migrated schema, an upstream that counts its calls.
// The agent's usual rate is seeded into its log as a week of spending one question an hour; it then asks
// question after question, and the alert must come on exactly the question that brings its last hour to
// five questions' worth, counting that question's hold.
func TestB196_AnAgentSpendingFiveTimesItsUsualRateIsFlaggedAndPaused(t *testing.T) {
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
	seamFund(t, pool, ws, 1_000_000_000)
	newAgent := func(name, key string) string {
		t.Helper()
		a, err := store.CreateAgent(ctx, ws, name, "user-owner")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.AttachAgentKey(ctx, ws, a.ID, key); err != nil {
			t.Fatal(err)
		}
		if _, err := store.FundAgent(ctx, ws, a.ID, 400_000_000); err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	ask := func(key string, stream bool) (int, string) {
		t.Helper()
		body := fmt.Sprintf(`{"model":"gpt-4o","stream":%t,"messages":[{"role":"user","content":%q}]}`, stream, strings.Repeat("q", 40000))
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Talyvor-Workspace", ws)
		req.Header.Set(CacheBypassHeader, "bypass")
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: key, WorkspaceID: ws}))
		w := httptest.NewRecorder()
		p.HandleOpenAI(w, req)
		return w.Code, w.Body.String()
	}
	balanceOf := func(id string) int64 {
		t.Helper()
		book, err := store.AgentBook(ctx, ws)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range book.Agents {
			if a.ID == id {
				return a.BalanceULXC
			}
		}
		t.Fatalf("no agent %s", id)
		return 0
	}
	alerts := func() []economy.AgentSpendAlert {
		t.Helper()
		list, err := store.ListAgentSpendAlerts(ctx, ws)
		if err != nil {
			t.Fatal(err)
		}
		return list
	}

	// What one question costs, and holds, on an agent with no pattern yet — which raises nothing.
	probe := newAgent("probe", "key-probe")
	if code, body := ask("key-probe", false); code != http.StatusOK {
		t.Fatalf("the probe's question = %d %s", code, body)
	}
	charge := 400_000_000 - balanceOf(probe)
	hold := reserveEstimateLXC("gpt-4o", strings.Repeat("q", 40000), 4096)
	if charge <= 0 || hold <= charge || hold >= economy.UnusualSpendMultiple*charge {
		t.Fatalf("a question charged %d µLXC and holds %d — want 0 < charge < hold < 5 charges", charge, hold)
	}
	if n := len(alerts()); n != 0 {
		t.Fatalf("an agent with no spending pattern raised %d alerts", n)
	}

	// The worker's usual rate: one question an hour for the week before the last hour.
	worker := newAgent("worker", "key-agent")
	if _, err := store.SetAgentRules(ctx, ws, worker, economy.AgentRules{PauseOnUnusualSpend: true}); err != nil {
		t.Fatal(err)
	}
	for d := 0; d < 7; d++ {
		at := time.Now().Add(-13*time.Hour - time.Duration(d)*24*time.Hour)
		entry := uuid.New()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, leg := range []struct {
			account string
			amount  int64
		}{{"agent:" + worker, -24 * charge}, {"spend", 24 * charge}} {
			if _, err := tx.Exec(ctx, `INSERT INTO agent_postings (entry_id, workspace_id, account, amount_ulxc, kind, ref, created_at)
				VALUES ($1, $2, $3, $4, 'spend', 'usual', $5)`, entry, ws, leg.account, leg.amount, at); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// Question n holds with n−1 questions already spent this hour: the first whose hold brings the hour to
	// five questions' worth raises the alert.
	flagged := 1
	for int64(flagged-1)*charge+hold < economy.UnusualSpendMultiple*charge {
		flagged++
	}
	for n := 1; n <= flagged; n++ {
		if code, body := ask("key-agent", false); code != http.StatusOK {
			t.Fatalf("question %d = %d %s — the question that raises the alert is still served", n, code, body)
		}
		if got, want := len(alerts()), map[bool]int{true: 1, false: 0}[n == flagged]; got != want {
			t.Fatalf("after question %d (%d µLXC in the hour with its hold, usual %d an hour): %d alerts, want %d",
				n, int64(n-1)*charge+hold, charge, got, want)
		}
	}
	a := alerts()[0]
	if a.AgentID != worker || !a.Paused || a.UsualPerHourULXC != charge || a.LastHourULXC != int64(flagged-1)*charge+hold {
		t.Errorf("the alert = %+v, want the worker paused at %d µLXC in the hour against a usual %d", a, int64(flagged-1)*charge+hold, charge)
	}

	// Paused: its next request is refused, buffered and streamed, before the provider is called and with
	// nothing charged.
	ledgerRows := func() (n int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM lxc_ledger WHERE workspace_id = $1`, ws).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, stream := range []bool{false, true} {
		calls, balance, rows := upstreamCalls.Load(), balanceOf(worker), ledgerRows()
		code, body := ask("key-agent", stream)
		if code != http.StatusForbidden || !strings.Contains(body, "paused") {
			t.Errorf("the paused agent's request (stream=%t) = %d %s, want 403 saying it is paused", stream, code, body)
		}
		if upstreamCalls.Load() != calls || balanceOf(worker) != balance || ledgerRows() != rows {
			t.Errorf("the paused agent's request (stream=%t) reached the provider or was charged", stream)
		}
	}

	// Resumed by its owner, it is served again, and the same hour raises no second alert.
	if err := store.ResumeAgent(ctx, ws, worker); err != nil {
		t.Fatal(err)
	}
	if code, body := ask("key-agent", false); code != http.StatusOK {
		t.Fatalf("the resumed agent = %d %s", code, body)
	}
	if n := len(alerts()); n != 1 {
		t.Errorf("%d alerts after the resumed agent spent again within the hour, want still 1", n)
	}
}
