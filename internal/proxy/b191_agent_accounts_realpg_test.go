package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/workspace"
)

// B19.1 — EVERY AGENT SPENDS FROM ITS OWN BALANCE, AND THE BOOKS RECONCILE TO THE MICRO-UNIT.
//
// A migrated schema (the double-entry trigger and append-only guard are real), the real handler with
// the agent reservation on. The workspace funds two agents; each asks one question with its own key
// and is charged from its own balance; a third agent funded below the hold is refused while the
// workspace still has LXC. Then workspace, agents and ledger must agree exactly.

func agentBankDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG agent-accounts test")
	}
	pool, err := pgxpool.New(context.Background(), migratedDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestB191_EachAgentSpendsItsOwnBalance_AndWorkspaceAgentsAndLedgerReconcile(t *testing.T) {
	pool := agentBankDB(t)
	ctx := context.Background()
	store := economy.NewDualTokenStore(nil, pool, nil)
	p, _, _ := newLoggingProxy(t, workspace.LoggingFull)
	p.router = nil
	p.SetAgentSpender(store, func() bool { return true })
	p.SetReservation(func() bool { return true }, func() int { return 4096 })
	p.openAIURL = usageUpstream(t, `{"prompt_tokens":10000,"completion_tokens":100}`).URL

	const ws, funded = "ws-log", int64(100_000_000) // 100 LXC, cash-backed
	seamFund(t, pool, ws, funded)

	newAgent := func(name, key string, fund int64) string {
		t.Helper()
		a, err := store.CreateAgent(ctx, ws, name, "user-owner")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.AttachAgentKey(ctx, ws, a.ID, key); err != nil {
			t.Fatal(err)
		}
		if _, err := store.FundAgent(ctx, ws, a.ID, fund); err != nil {
			t.Fatalf("fund %s: %v", name, err)
		}
		return a.ID
	}
	researcher := newAgent("researcher", "key-researcher", 10_000_000) // 10 LXC
	writer := newAgent("writer", "key-writer", 5_000_000)              // 5 LXC
	starved := newAgent("starved", "key-starved", 100_000)             // 0.1 LXC — below any hold

	// Each agent asks its own question (the same length, so the same price): a repeat would be answered
	// from the workspace's cache, free.
	ask := func(key string) int {
		t.Helper()
		body := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}]}`, key+strings.Repeat("x", 40000-len(key)))
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Talyvor-Workspace", ws)
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: key, WorkspaceID: ws}))
		w := httptest.NewRecorder()
		p.HandleOpenAI(w, req)
		return w.Code
	}
	balances := func() map[string]int64 {
		t.Helper()
		book, err := store.AgentBook(ctx, ws)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int64{"workspace": book.WorkspaceBalanceULXC, "allocated": book.AllocatedULXC,
			"unallocated": book.UnallocatedULXC, "spent": book.SpentULXC}
		for _, a := range book.Agents {
			out[a.ID] = a.BalanceULXC
		}
		return out
	}

	if code := ask("key-researcher"); code != http.StatusOK {
		t.Fatalf("researcher's question = %d, want 200", code)
	}
	afterOne := balances()
	chargeR := 10_000_000 - afterOne[researcher]
	if chargeR <= 0 || afterOne[writer] != 5_000_000 {
		t.Fatalf("after the researcher asked: researcher charged %d µLXC, writer holds %d (want >0 and untouched 5,000,000)",
			chargeR, afterOne[writer])
	}
	if code := ask("key-writer"); code != http.StatusOK {
		t.Fatalf("writer's question = %d, want 200", code)
	}
	if code := ask("key-starved"); code != http.StatusPaymentRequired {
		t.Fatalf("an agent funded below the hold got %d, want 402 — it spent beyond its own balance", code)
	}

	b := balances()
	chargeW := 5_000_000 - b[writer]
	if chargeW != chargeR {
		t.Errorf("the same question cost the researcher %d and the writer %d µLXC", chargeR, chargeW)
	}
	if b[starved] != 100_000 {
		t.Errorf("the refused agent holds %d µLXC, want its 100,000 untouched", b[starved])
	}
	// WORKSPACE: its LXC fell by exactly what its agents spent, and the lxc_ledger says the same.
	if b["workspace"] != funded-chargeR-chargeW {
		t.Errorf("workspace balance %d, want %d − %d − %d", b["workspace"], funded, chargeR, chargeW)
	}
	var ledgerNet int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount), 0)::bigint FROM lxc_ledger WHERE workspace_id = $1`, ws).Scan(&ledgerNet); err != nil {
		t.Fatal(err)
	}
	if ledgerNet != -(chargeR + chargeW) {
		t.Errorf("lxc_ledger nets %d µLXC, want −%d", ledgerNet, chargeR+chargeW)
	}
	// AGENTS: allocated is their balances, spent is the 'spend' account, and together with what is
	// unallocated they are the workspace's balance.
	if b["allocated"] != b[researcher]+b[writer]+b[starved] || b["spent"] != chargeR+chargeW ||
		b["unallocated"]+b["allocated"] != b["workspace"] {
		t.Errorf("the book does not reconcile: %+v (charges %d + %d)", b, chargeR, chargeW)
	}
	// LEDGER: every entry sums to zero, so all postings do.
	var postings, entries, unbalanced int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount_ulxc), 0)::bigint, count(DISTINCT entry_id),
		(SELECT count(*) FROM (SELECT entry_id FROM agent_postings GROUP BY entry_id HAVING sum(amount_ulxc) <> 0) u)
		FROM agent_postings`).Scan(&postings, &entries, &unbalanced); err != nil {
		t.Fatal(err)
	}
	if postings != 0 || unbalanced != 0 {
		t.Errorf("postings sum to %d over %d entries, %d unbalanced — want 0 and 0", postings, entries, unbalanced)
	}
	// And the record cannot be edited.
	if _, err := pool.Exec(ctx, `UPDATE agent_postings SET amount_ulxc = amount_ulxc + 1`); err == nil {
		t.Error("a posting was edited — the agent ledger must be append-only")
	}
}
