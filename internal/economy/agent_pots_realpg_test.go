package economy

import (
	"context"
	"errors"
	"testing"
	"time"
)

// B22.7 — on the migrated schema: an agent holding 100 LXC makes a goal pot and a reserve locked for a day,
// moves 30 and 20 LXC in and 10 back out of the goal. The locked reserve refuses a withdrawal before its date
// and gives it after. What is in a pot is the agent's but not spendable, by it or by the workspace, and its
// statement shows each pot and each move.
func TestAgentPots_TwoPotsMovesAndALock(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := NewDualTokenStore(nil, pool, nil)
	const lxc = int64(1_000_000)
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ('ws-p', 'ws-p', 'ws-p')`); err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateAgent(ctx, "ws-p", "saver", "user-p")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreditLXC(ctx, "ws-p", 100*lxc, "top-up", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FundAgent(ctx, "ws-p", a.ID, 100*lxc); err != nil {
		t.Fatal(err)
	}
	if err := s.AttachAgentKey(ctx, "ws-p", a.ID, "key-saver"); err != nil {
		t.Fatal(err)
	}
	tomorrow := time.Now().Add(24 * time.Hour)
	goal, err := s.CreatePot(ctx, "ws-p", a.ID, "new laptop", "goal", 50*lxc, nil)
	if err != nil {
		t.Fatal(err)
	}
	reserve, err := s.CreatePot(ctx, "ws-p", a.ID, "rainy day", "reserve", 0, &tomorrow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePot(ctx, "ws-p", a.ID, "new laptop", "budget", 0, nil); !errors.Is(err, ErrPot) {
		t.Fatalf("a second pot of the same name = %v, want refused", err)
	}
	for _, m := range []struct {
		pot    Pot
		amount int64
		in     bool
	}{{goal, 30 * lxc, true}, {reserve, 20 * lxc, true}, {goal, 10 * lxc, false}} {
		move := s.MoveToPot
		if !m.in {
			move = s.MoveFromPot
		}
		if _, err := move(ctx, "ws-p", a.ID, m.pot.ID, m.amount); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.MoveFromPot(ctx, "ws-p", a.ID, reserve.ID, 5*lxc); !errors.Is(err, ErrPotLocked) {
		t.Fatalf("taking from the locked reserve = %v, want refused until its date", err)
	}
	if _, err := s.LockPot(ctx, "ws-p", a.ID, reserve.ID, nil); !errors.Is(err, ErrPotLocked) {
		t.Fatalf("lifting the reserve's lock before its date = %v, want refused", err)
	}

	// The agent spends only its main balance (60 LXC); the pots are not the workspace's to spend either.
	if err := s.SpendLXCForAgent(ctx, "key-saver", "ws-p", "req-1", 61*lxc, "a call", AgentDebitMeta{}); !errors.Is(err, ErrSubBudgetExceeded) {
		t.Fatalf("spending past the main balance = %v, want refused: the pots are not spendable", err)
	}
	if free, err := s.GetUnallocatedLXC(ctx, "ws-p"); err != nil || free != 0 {
		t.Fatalf("unallocated = %d, %v; want 0", free, err)
	}
	book, err := s.AgentBook(ctx, "ws-p")
	if err != nil || book.Agents[0].BalanceULXC != 60*lxc || book.Agents[0].PotsULXC != 40*lxc || book.UnallocatedULXC != 0 {
		t.Fatalf("book = %+v, %v; want 60 LXC to spend and 40 LXC in pots", book, err)
	}

	// The statement shows each pot and each move.
	st, err := s.AgentPeriodStatement(ctx, "ws-p", a.ID, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil || len(st.Pots) != 2 || st.Pots[0].BalanceULXC != 20*lxc || st.Pots[1].BalanceULXC != 20*lxc {
		t.Fatalf("statement pots = %+v, %v; want the goal and the reserve at 20 LXC each", st.Pots, err)
	}
	var moves int
	for _, l := range st.Lines {
		if l.Kind == "pot_in" || l.Kind == "pot_out" {
			moves++
		}
	}
	if moves != 3 {
		t.Errorf("the statement has %d pot moves, want 3", moves)
	}

	// Its date passed, the reserve gives its credits back.
	if _, err := pool.Exec(ctx, `UPDATE agent_pots SET locked_until = now() - interval '1 second' WHERE id = $1`, reserve.ID); err != nil {
		t.Fatal(err)
	}
	if p, err := s.MoveFromPot(ctx, "ws-p", a.ID, reserve.ID, 20*lxc); err != nil || p.BalanceULXC != 0 {
		t.Fatalf("taking from the reserve after its date = %+v, %v", p, err)
	}
}
