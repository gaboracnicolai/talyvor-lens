package proxy

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/fees"
	"github.com/talyvor/lens/internal/workspace"
)

// B32.11 — the platform fee through the real handler, on a migrated schema, buffered and streamed, asserted on
// lxc_ledger and agent_postings: an agent's question on Team pays its delivered cost and 3% of it, whether its
// charge is a hold and settle or a debit and settle; a question on the workspace's own provider key carries no
// fee; a chat question drawn from a Plus allowance carries none, and the part past the allowance carries 3%.

func b3211Fee(store *economy.DualTokenStore) {
	store.SetPlatformFee(func(ctx context.Context, q economy.FeeQuerier, ws string) (int64, error) {
		return billing.PlatformFeeBPS(ctx, q, ws, fees.Defaults())
	})
}

// b3211OnPlan puts ws on plan through a live subscription, as the webhook writes it; byok marks the add-on.
func b3211OnPlan(t *testing.T, pool *pgxpool.Pool, ws, plan string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO subscriptions (workspace_id, stripe_subscription_id,
		stripe_customer_id, price_id, status, livemode, last_event_at, plan, byok)
		VALUES ($1, 'sub_' || $1, 'cus_' || $1, 'price_' || $2, 'active', false, NOW(), $2, $2 = 'byok')`, ws, plan); err != nil {
		t.Fatal(err)
	}
}

// b3211Books is ws's net spend and net platform fee on lxc_ledger, the requests its fee rows name, and the
// platform fee its agents' postings carry.
func b3211Books(t *testing.T, pool *pgxpool.Pool, ws string) (spent, fee, agentFee int64, requests []string) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx, `SELECT COALESCE(-sum(amount) FILTER (WHERE type = 'spend'), 0)::bigint,
		COALESCE(-sum(amount) FILTER (WHERE type = 'platform_fee'), 0)::bigint,
		COALESCE(array_agg(DISTINCT metadata->>'request_id') FILTER (WHERE type = 'platform_fee'), '{}')
		FROM lxc_ledger WHERE workspace_id = $1`, ws).Scan(&spent, &fee, &requests); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT COALESCE(-sum(amount_ulxc), 0)::bigint FROM agent_postings
		WHERE workspace_id = $1 AND account LIKE 'agent:%' AND kind = 'platform_fee'`, ws).Scan(&agentFee); err != nil {
		t.Fatal(err)
	}
	return
}

func TestB3211_AnAgentsQuestionOnTeamPaysThePlatformFee(t *testing.T) {
	for _, reservations := range []bool{true, false} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("reservations=%t/streamed=%t", reservations, stream), func(t *testing.T) {
				p, store, pool, agentID := b2313Setup(t, stream, 10000, 100, nil)
				p.SetReservation(func() bool { return reservations }, func() int { return 4096 })
				b3211Fee(store)
				b3211OnPlan(t, pool, b2313WS, "team")

				b2313Ask(t, p, 0, stream)
				cost := settleULXC(alerts.CostUSD("gpt-4o", 10000, 100))
				spent, fee, agentFee, requests := b3211Books(t, pool, b2313WS)
				want := economy.PlatformFee(cost, 300)
				if spent != cost || fee != want || agentFee != want || len(requests) != 1 || requests[0] != "question-0" {
					t.Fatalf("spent %d with a %d fee (the agent's postings %d) naming %v — want %d with a %d fee, posted, naming question-0",
						spent, fee, agentFee, requests, cost, want)
				}
				var left int64
				if err := pool.QueryRow(context.Background(), `SELECT balance_ulxc FROM agent_account_balances
					WHERE workspace_id = $1 AND account = $2`, b2313WS, "agent:"+agentID).Scan(&left); err != nil || left != b2313Fund-cost-want {
					t.Errorf("the agent holds %d (%v), want %d: its 10 LXC less the question and its fee", left, err, b2313Fund-cost-want)
				}
			})
		}
	}
}

func TestB3211_AQuestionOnTheWorkspacesOwnKeyCarriesNoFee(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("streamed=%t", stream), func(t *testing.T) {
			const ownKey = "sk-own-b3211-ws-log-0001"
			p, store, pool, _ := b2313Setup(t, stream, 10000, 100, nil)
			p.SetReservation(func() bool { return true }, func() int { return 4096 })
			b3211Fee(store)
			up := &b2726Upstream{}
			p.openAIURL = up.serve(t)
			keys := b2726Store(t, pool)
			p.SetOwnKeys(keys)
			b3211OnPlan(t, pool, b2313WS, "byok")

			// The control: before the workspace stores its key, the question goes on Talyvor's and pays the fee.
			b2313Ask(t, p, 0, stream)
			_, fee0, _, _ := b3211Books(t, pool, b2313WS)
			if fee0 == 0 {
				t.Fatal("control: a question on Talyvor's key paid no platform fee")
			}
			if _, err := keys.Put(context.Background(), b2313WS, "openai", ownKey); err != nil {
				t.Fatal(err)
			}
			b2313Ask(t, p, 1, stream)
			if n, auth := up.calls(); n != 2 || auth != "Bearer "+ownKey {
				t.Fatalf("%d upstream calls, the last on %q — want the second on the workspace's own key", n, auth)
			}
			if _, fee1, _, _ := b3211Books(t, pool, b2313WS); fee1 != fee0 {
				t.Errorf("the platform fee went from %d to %d on a question on the workspace's own key, want no fee row", fee0, fee1)
			}
		})
	}
}

func TestB3211_AChatQuestionDrawnFromAPlusAllowanceCarriesNoFee_PastItItDoes(t *testing.T) {
	const allowance = int64(300_000)
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("streamed=%t", stream), func(t *testing.T) {
			pool := agentBankDB(t)
			ctx := context.Background()
			store := economy.NewDualTokenStore(nil, pool, nil)
			b3211Fee(store)
			p, _, _ := newLoggingProxy(t, workspace.LoggingFull)
			p.router = nil
			p.SetAgentSpender(store, func() bool { return true })
			p.SetReservation(func() bool { return true }, func() int { return 4096 })
			p.SetLXCSpendSink(store, func() bool { return false })
			p.SetLXCGate(store, func() bool { return false })
			seamFund(t, pool, "ws-log", costWireFunded)
			b3211OnPlan(t, pool, "ws-log", "plus")
			svc := billing.New(pool, store, nil, "").WithAllowance(allowance)
			if _, err := svc.Grant(ctx, "ws-log", "sub_ws-log", time.Now().Add(-time.Hour), time.Now().Add(30*24*time.Hour)); err != nil {
				t.Fatal(err)
			}
			p.SetSubscriptionAllowance(svc)
			var calls int64
			chatUpstream(t, p, stream, &calls)
			cost := settleULXC(alerts.CostUSD("gpt-4o", 10000, 100))
			if cost >= allowance || 2*cost <= allowance {
				t.Fatalf("fixture: one question (%d) must fit the allowance (%d) and two must not", cost, allowance)
			}

			if code := driveWithAuth(t, p, sessionKeyAuthContext(t, "ws-log"), stream, "b3211-1"); code != http.StatusOK {
				t.Fatalf("status %d, want 200", code)
			}
			if spent, fee, _, _ := b3211Books(t, pool, "ws-log"); atomic.LoadInt64(&calls) != 1 || spent != 0 || fee != 0 {
				t.Fatalf("a question within the allowance: %d upstream calls, %d prepaid spend, %d fee — want 1 call, all from the allowance, no fee row",
					calls, spent, fee)
			}

			if code := driveWithAuth(t, p, sessionKeyAuthContext(t, "ws-log"), stream, "b3211-2"); code != http.StatusOK {
				t.Fatalf("status %d, want 200", code)
			}
			past := 2*cost - allowance
			if spent, fee, _, _ := b3211Books(t, pool, "ws-log"); spent != past || fee != economy.PlatformFee(past, 300) {
				t.Errorf("past the allowance: %d prepaid with a %d fee, want %d with %d (Plus takes Team's 3%%)",
					spent, fee, past, economy.PlatformFee(past, 300))
			}
		})
	}
}
