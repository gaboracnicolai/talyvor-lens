package proxy

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/migrations"
)

// B1.6 — the browser chat (a SESSION KEY: no APIKeyID, so no reservation and, by default, no
// LXC movement at all — session_key_billing_realpg_test.go) draws a subscriber's plan
// allowance, and pays from prepaid only past it. Driven through the real handler in the
// DEFAULT billing configuration (reservations on, shadow debit off), on BOTH seams, and
// asserted on the allowance row and the LXC ledger.

const testAllowanceULXC = int64(100_000)

// subscriberProxy is costWireProxy plus a real billing.Service as the allowance, wired the
// way cmd/lens wires it: the prepaid sink and reader present, the shadow flag OFF.
func subscriberProxy(t *testing.T, prepaid int64) (*Proxy, *billing.Service, *pgxpool.Pool) {
	t.Helper()
	p, store, pool := costWireProxy(t)
	ctx := context.Background()
	for _, f := range []string{"0121_subscription_allowance.sql", "0127_subscription_allowance_fee.sql"} {
		ddl, err := migrations.FS.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(ddl)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM subscription_allowance WHERE workspace_id = 'ws-log'`); err != nil {
		t.Fatal(err)
	}
	seamFund(t, pool, "ws-log", prepaid)
	svc := billing.New(pool, store, nil, "").WithAllowance(testAllowanceULXC)
	now := time.Now()
	if _, err := svc.Grant(ctx, "ws-log", "sub_b16", now.Add(-time.Hour), now.Add(30*24*time.Hour)); err != nil {
		t.Fatalf("grant: %v", err)
	}
	p.SetLXCSpendSink(store, func() bool { return false })
	p.SetLXCGate(store, func() bool { return false })
	p.SetSubscriptionAllowance(svc)
	return p, svc, pool
}

func chatUpstream(t *testing.T, p *Proxy, stream bool, calls *int64) {
	t.Helper()
	if stream {
		sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10000,\"completion_tokens\":100}}\n\n" +
			"data: [DONE]\n\n"
		p.openAIURL = countingUpstream(t, sseUpstream(t, sse), calls).URL
		return
	}
	p.openAIURL = countingUpstream(t, usageUpstream(t, `{"prompt_tokens":10000,"completion_tokens":100}`), calls).URL
}

func TestAllowance_SessionKeyChatDrawsThePlanThenPrepaid_BothSeams(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffered", true: "streamed"}[stream], func(t *testing.T) {
			p, svc, pool := subscriberProxy(t, costWireFunded)
			var calls int64
			chatUpstream(t, p, stream, &calls)

			if code := driveWithAuth(t, p, sessionKeyAuthContext(t, "ws-log"), stream, "b16-draw"); code != http.StatusOK {
				t.Fatalf("status = %d, want 200", code)
			}
			if atomic.LoadInt64(&calls) != 1 {
				t.Fatalf("upstream calls = %d, want 1", calls)
			}
			cost := settleULXC(alerts.CostUSD("gpt-4o", 10000, 100))
			if cost <= testAllowanceULXC {
				t.Fatalf("fixture: cost %d must exceed the allowance %d to exercise the prepaid remainder", cost, testAllowanceULXC)
			}
			a, err := svc.CurrentAllowance(context.Background(), "ws-log", time.Now())
			if err != nil || a == nil {
				t.Fatalf("allowance row = %+v, %v", a, err)
			}
			if a.ConsumedULXC != testAllowanceULXC {
				t.Errorf("allowance consumed = %d, want %d — the whole allowance, and not a µLXC more", a.ConsumedULXC, testAllowanceULXC)
			}
			var debit int64
			var rows int
			if err := pool.QueryRow(context.Background(), `SELECT COALESCE(SUM(-amount),0), COUNT(*) FROM lxc_ledger
				WHERE workspace_id='ws-log' AND description='subscription: usage beyond the plan allowance'`).Scan(&debit, &rows); err != nil {
				t.Fatal(err)
			}
			if rows != 1 || debit != cost-testAllowanceULXC {
				t.Errorf("prepaid ledger: %d row(s) debiting %d, want 1 debiting %d (cost %d − allowance %d)",
					rows, debit, cost-testAllowanceULXC, cost, testAllowanceULXC)
			}
		})
	}
}

// HARD CAP: allowance used up and no prepaid ⇒ refused before the provider is called.
func TestAllowance_UsedUpWithNoPrepaid_RefusedBeforeUpstream_BothSeams(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffered", true: "streamed"}[stream], func(t *testing.T) {
			p, svc, _ := subscriberProxy(t, 0)
			if _, err := svc.Consume(context.Background(), "ws-log", testAllowanceULXC, time.Now()); err != nil {
				t.Fatal(err)
			}
			var calls int64
			chatUpstream(t, p, stream, &calls)
			if code := driveWithAuth(t, p, sessionKeyAuthContext(t, "ws-log"), stream, "b16-cap"); code != http.StatusPaymentRequired {
				t.Fatalf("status = %d, want 402", code)
			}
			if n := atomic.LoadInt64(&calls); n != 0 {
				t.Fatalf("upstream calls = %d, want 0 — a refused request must not reach the provider", n)
			}
		})
	}
}
