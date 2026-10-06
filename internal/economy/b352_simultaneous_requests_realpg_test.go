package economy

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// B35.2 — an agent's simultaneous requests no longer refuse each other or go unbilled. Before it, a hold took
// the key's sub-budget, then the agent, then the workspace balance, while a settle took the balance first: a
// hold and a settle of one agent at the same moment deadlocked, Postgres cancelled one, a cancelled hold was a
// 402 with money in the wallet and a cancelled settle left the answer to the stranded sweeper, free.

const (
	b352Requests = 60  // one agent's questions…
	b352AtOnce   = 10  // …sent this many at a time, as agent-request-rate sends them
	b352Hold     = 920 // the estimate each holds, before its platform fee (claude-haiku-4-5, max_tokens 16)
	b352Charge   = 540 // what each answer cost
	b352FeeBPS   = 550 // Free's 5.5%
)

// b352Store is a store on its own pool, wide enough that every request in flight has a connection, with the
// platform fee on so each settle writes its fee row.
func b352Store(t *testing.T) (*DualTokenStore, *pgxpool.Pool) {
	t.Helper()
	cfg := supplyPool(t).Config().Copy()
	cfg.MaxConns = 4 * b352AtOnce
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s := NewDualTokenStore(nil, pool, nil)
	s.SetPlatformFee(func(context.Context, FeeQuerier, string) (int64, error) { return b352FeeBPS, nil })
	return s, pool
}

// b352Agent creates a funded agent with a key and its owner's 60-a-minute rule, and returns the agent and key.
func b352Agent(t *testing.T, s *DualTokenStore, ws, name string) (Agent, string) {
	t.Helper()
	ctx := context.Background()
	a, err := s.CreateAgent(ctx, ws, name, "user-"+name)
	if err != nil {
		t.Fatal(err)
	}
	key := "key-" + name
	if err := s.AttachAgentKey(ctx, ws, a.ID, key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FundAgent(ctx, ws, a.ID, 1_000_000); err != nil { // 1 LXC, as the scenario funds it
		t.Fatal(err)
	}
	perMinute := int64(b352Requests)
	if _, err := s.SetAgentRules(ctx, ws, a.ID, AgentRules{RequestsPerMinute: &perMinute}); err != nil {
		t.Fatal(err)
	}
	return a, key
}

// b352Ask sends one key's 60 questions 10 at a time, each a hold then a settle, and returns every error.
func b352Ask(s *DualTokenStore, ws, key string) []error {
	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	next := make(chan int)
	go func() {
		for i := 0; i < b352Requests; i++ {
			next <- i
		}
		close(next)
	}()
	for w := 0; w < b352AtOnce; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				id := fmt.Sprintf("%s-res-%02d", key, i)
				ctx := WithAgentRequest(context.Background(), AgentRequest{Model: "claude-haiku-4-5", Provider: "anthropic"})
				meta := AgentDebitMeta{RequestedModel: "claude-haiku-4-5", RequestID: id}
				err := s.ReserveLXCForAgent(ctx, key, ws, id, b352Hold, meta)
				if err == nil {
					_, _, err = s.SettleLXCReservation(context.Background(), id, b352Charge,
						AgentDebitMeta{ServedModel: "claude-haiku-4-5"})
				}
				if err != nil {
					mu.Lock()
					errs = append(errs, fmt.Errorf("%s: %w", id, err))
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	return errs
}

// b352RequireBilled checks the book after every question was answered: each held, each settled, each charged
// once with its own fee row, none refunded in full, and both the agents' and the workspace's balances are the
// sums of their postings.
func b352RequireBilled(t *testing.T, pool *pgxpool.Pool, ws string, credited int64, agents []Agent) {
	t.Helper()
	ctx := context.Background()
	n := int64(b352Requests * len(agents))
	fee := PlatformFee(b352Charge, b352FeeBPS)
	count := func(what, q string, args ...any) int64 {
		t.Helper()
		var c int64
		if err := pool.QueryRow(ctx, q, args...).Scan(&c); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		return c
	}
	for _, c := range []struct {
		what string
		got  int64
	}{
		{"settled reservations", count("settled", `SELECT count(*) FROM lxc_reservations WHERE workspace_id = $1 AND status = 'settled'`, ws)},
		{"hold rows", count("holds", `SELECT count(*) FROM lxc_ledger WHERE workspace_id = $1 AND type = 'reservation_hold'`, ws)},
		{"spend rows", count("spends", `SELECT count(*) FROM lxc_ledger WHERE workspace_id = $1 AND type = 'spend'`, ws)},
		{"platform_fee rows", count("fees", `SELECT count(*) FROM lxc_ledger WHERE workspace_id = $1 AND type = 'platform_fee'`, ws)},
		{"hold postings", count("hold postings", `SELECT count(*) FROM agent_postings WHERE workspace_id = $1 AND kind = 'hold' AND account LIKE 'agent:%'`, ws)},
		{"settle postings", count("settle postings", `SELECT count(*) FROM agent_postings WHERE workspace_id = $1 AND kind = 'settle' AND account LIKE 'agent:%'`, ws)},
	} {
		if c.got != n {
			t.Errorf("%s = %d, want %d", c.what, c.got, n)
		}
	}
	if left := count("unresolved", `SELECT count(*) FROM lxc_reservations WHERE workspace_id = $1 AND status <> 'settled'`, ws); left != 0 {
		t.Errorf("%d reservations are still held or were released in full, want none", left)
	}
	if wrong := count("charges", `SELECT count(*) FROM lxc_ledger WHERE workspace_id = $1 AND type = 'spend' AND amount <> $2`, ws, -b352Charge); wrong != 0 {
		t.Errorf("%d spend rows are not the answer's %d µLXC", wrong, b352Charge)
	}

	// The workspace's balance is the sum of its ledger, and is what it was credited less every answer and its fee.
	var bal, ledger int64
	if err := pool.QueryRow(ctx, `SELECT balance, (SELECT COALESCE(sum(amount), 0)::bigint FROM lxc_ledger WHERE workspace_id = $1)
		FROM lxc_balances WHERE workspace_id = $1`, ws).Scan(&bal, &ledger); err != nil {
		t.Fatal(err)
	}
	if want := credited - n*(b352Charge+fee); bal != ledger || bal != want {
		t.Errorf("workspace balance %d, its ledger sums to %d, want both %d", bal, ledger, want)
	}
	// Each agent's stored balance is the sum of its postings, and is its funding less its answers and their fees.
	requireReconciled(t, ctx, pool)
	for _, a := range agents {
		var got int64
		if err := pool.QueryRow(ctx, balanceSQL, ws, agentAccount(a.ID)).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if want := 1_000_000 - int64(b352Requests)*(b352Charge+fee); got != want {
			t.Errorf("agent %s holds %d µLXC, want %d", a.Name, got, want)
		}
	}
}

func b352Workspace(t *testing.T, s *DualTokenStore, pool *pgxpool.Pool, ws string, credit int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreditLXC(ctx, ws, credit, "top-up", nil); err != nil {
		t.Fatal(err)
	}
}

// One agent, funded 1 LXC, asks 60 questions 10 at a time: every one holds and settles, none is refused.
func TestB352_AnAgentsSimultaneousRequestsAllHoldAndSettle(t *testing.T) {
	s, pool := b352Store(t)
	const ws, credit = "ws-b352-one", 10_000_000
	b352Workspace(t, s, pool, ws, credit)
	a, key := b352Agent(t, s, ws, "rate")
	if errs := b352Ask(s, ws, key); len(errs) > 0 {
		t.Fatalf("%d of %d questions failed, first: %v", len(errs), b352Requests, errs[0])
	}
	b352RequireBilled(t, pool, ws, credit, []Agent{a})
}

// Two agents of one workspace ask theirs at the same moment: they share the workspace's balance and its fee
// side, and every question of both still holds and settles.
func TestB352_TwoAgentsOfOneWorkspaceAskAtOnce(t *testing.T) {
	s, pool := b352Store(t)
	const ws, credit = "ws-b352-two", 10_000_000
	b352Workspace(t, s, pool, ws, credit)
	a, keyA := b352Agent(t, s, ws, "alpha")
	b, keyB := b352Agent(t, s, ws, "beta")
	var errsA, errsB []error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); errsA = b352Ask(s, ws, keyA) }()
	go func() { defer wg.Done(); errsB = b352Ask(s, ws, keyB) }()
	wg.Wait()
	if errs := append(errsA, errsB...); len(errs) > 0 {
		t.Fatalf("%d of %d questions failed, first: %v", len(errs), 2*b352Requests, errs[0])
	}
	b352RequireBilled(t, pool, ws, credit, []Agent{a, b})
}
