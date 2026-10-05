package economy

import (
	"context"
	"errors"
	"testing"
	"time"
)

// B28.302 — an agent's owner caps its requests a minute, judged inside its hold or debit like its other rules: under
// a 3/min rule the 4th question in a minute holds nothing, a debit's later under-charge settle is not another
// question, and a minute on the agent may ask again.
func TestB28302_TheRequestOverAnAgentsPerMinuteCapHoldsNothing(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := NewDualTokenStore(nil, pool, nil)
	const ws, key, model = "ws-rpm", "key-rpm", "claude-haiku-4-5"
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreditLXC(ctx, ws, 100_000_000, "top-up", nil); err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateAgent(ctx, ws, "rate capped", "user-rpm")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AttachAgentKey(ctx, ws, a.ID, key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FundAgent(ctx, ws, a.ID, 50_000_000); err != nil {
		t.Fatal(err)
	}
	three := int64(3)
	if rules, err := s.SetAgentRules(ctx, ws, a.ID, AgentRules{RequestsPerMinute: &three}); err != nil || limitOf(rules.RequestsPerMinute) != 3 {
		t.Fatalf("saved rules = %+v, %v — want 3 requests a minute", rules, err)
	}
	asking := func(at time.Time) context.Context {
		return WithAgentRequest(ctx, AgentRequest{Model: model, Provider: "anthropic", At: at})
	}
	meta := AgentDebitMeta{RequestedModel: model}
	postings := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_postings WHERE workspace_id = $1`, ws).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Question 1 is a debit settled for more than it estimated: its settle is a second 'spend' of the same ref.
	if err := s.SpendLXCForAgent(asking(time.Time{}), key, ws, "req-1", 1_000_000, "q1", meta); err != nil {
		t.Fatalf("question 1: %v", err)
	}
	if _, err := s.SettleAgentDebit(ctx, ws, "req-1", 2_000_000, meta); err != nil {
		t.Fatal(err)
	}
	var spends int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_postings WHERE ref = 'req-1' AND account = $1 AND kind = 'spend'`,
		agentAccount(a.ID)).Scan(&spends); err != nil || spends != 2 {
		t.Fatalf("question 1 and its settle wrote %d 'spend' postings on the agent's account (%v), want 2", spends, err)
	}
	// Questions 2 and 3 are holds; the settle above did not use one of them up.
	for _, ref := range []string{"res-2", "res-3"} {
		if err := s.ReserveLXCForAgent(asking(time.Time{}), key, ws, ref, 1_000_000, meta); err != nil {
			t.Fatalf("question %s, within 3 a minute: %v", ref, err)
		}
	}

	// Question 4 in the same minute is refused as the rate rule's, and nothing is written anywhere in the book.
	before := postings()
	err = s.ReserveLXCForAgent(asking(time.Time{}), key, ws, "res-4", 1_000_000, meta)
	if !errors.Is(err, ErrAgentRequestRate) || !errors.Is(err, ErrAgentRule) {
		t.Fatalf("question 4 in a minute under a 3/min rule = %v, want the requests-per-minute refusal", err)
	}
	if n := postings(); n != before {
		t.Fatalf("question 4 wrote %d postings, want none", n-before)
	}
	var reservations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM lxc_reservations WHERE reservation_id = 'res-4'`).Scan(&reservations); err != nil || reservations != 0 {
		t.Fatalf("question 4 left %d reservations (%v), want none", reservations, err)
	}

	// A minute on, none of the three is in the last sixty seconds: the same question holds.
	if err := s.ReserveLXCForAgent(asking(time.Now().Add(61*time.Second)), key, ws, "res-5", 1_000_000, meta); err != nil {
		t.Fatalf("a question a minute later: %v", err)
	}
}
