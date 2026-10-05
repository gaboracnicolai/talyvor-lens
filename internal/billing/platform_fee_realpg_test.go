package billing

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/fees"
)

// B32.11 — a platform fee on AI spend, by plan: on a migrated schema, asserted on lxc_ledger and agent_postings.
// A 10 LXC call writes a −10,000,000 µLXC spend row and its plan's platform_fee row in the same transaction
// (the same xmin); an agent with 10.2 LXC left on Team is refused a 10 LXC call, which costs 10.3 with the fee.

func feeStore(t *testing.T) (*economy.DualTokenStore, *pgxpool.Pool) {
	t.Helper()
	_, pool, dt := newBillingService(t)
	dt.SetPlatformFee(func(ctx context.Context, q economy.FeeQuerier, ws string) (int64, error) {
		return PlatformFeeBPS(ctx, q, ws, fees.Defaults())
	})
	return dt, pool
}

// onPlan seeds a workspace with 100 LXC, on plan ("" is none) through a live subscription as the webhook writes it.
func onPlan(t *testing.T, dt *economy.DualTokenStore, pool *pgxpool.Pool, plan string) string {
	t.Helper()
	ctx := context.Background()
	ws := fmt.Sprintf("ws-b3211-%s-%d", plan, time.Now().UnixNano())
	seedWS(t, pool, ws)
	if plan != "" {
		if _, err := pool.Exec(ctx, `INSERT INTO subscriptions (workspace_id, stripe_subscription_id, stripe_customer_id,
			price_id, status, livemode, last_event_at, plan) VALUES ($1, 'sub_' || $1, 'cus_' || $1, 'price_' || $2, 'active', false, NOW(), $2)`,
			ws, plan); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := dt.CreditLXC(ctx, ws, 100_000_000, "top-up", nil); err != nil {
		t.Fatal(err)
	}
	return ws
}

type feeRow struct {
	typ, xmin, requestID string
	amount, bps          int64
}

// spendAndFee is ws's spend and platform_fee rows, oldest first.
func spendAndFee(t *testing.T, pool *pgxpool.Pool, ws string) []feeRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT type, xmin::text, amount::bigint, COALESCE(metadata->>'request_id', ''),
		COALESCE((metadata->>'platform_fee_bps')::bigint, 0) FROM lxc_ledger
		WHERE workspace_id = $1 AND type IN ('spend', 'platform_fee') ORDER BY created_at, type DESC`, ws)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []feeRow
	for rows.Next() {
		var r feeRow
		if err := rows.Scan(&r.typ, &r.xmin, &r.amount, &r.requestID, &r.bps); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestB3211_ATenLXCCallWritesItsPlansPlatformFeeInTheSameTransaction(t *testing.T) {
	dt, pool := feeStore(t)
	ctx := context.Background()
	for _, tc := range []struct {
		plan string
		bps  int64
	}{{"", 550}, {"team", 300}, {"business", 100}, {"plus", 300}, {"byok", 300}} {
		t.Run("plan="+tc.plan, func(t *testing.T) {
			ws := onPlan(t, dt, pool, tc.plan)
			if _, err := dt.SpendLXCMeta(economy.WithChargeRequest(ctx, "req-"+ws), ws, 10_000_000, "chat: metered usage", nil); err != nil {
				t.Fatal(err)
			}
			rows := spendAndFee(t, pool, ws)
			fee := tc.bps * 1_000
			if len(rows) != 2 || rows[0].typ != "spend" || rows[0].amount != -10_000_000 ||
				rows[1].typ != "platform_fee" || rows[1].amount != -fee || rows[1].bps != tc.bps || rows[1].requestID != "req-"+ws {
				t.Fatalf("rows = %+v, want a −10,000,000 spend row and a −%d platform_fee row at %d bps naming req-%s", rows, fee, tc.bps, ws)
			}
			if rows[0].xmin != rows[1].xmin {
				t.Errorf("the spend row (xmin %s) and the fee row (xmin %s) were written in different transactions", rows[0].xmin, rows[1].xmin)
			}
			if bal, err := dt.GetLXCBalance(ctx, ws); err != nil || bal != 100_000_000-10_000_000-fee {
				t.Errorf("balance = %d, %v; want %d", bal, err, 100_000_000-10_000_000-fee)
			}
		})
	}

	t.Run("an Enterprise contract's own figure", func(t *testing.T) {
		ws := onPlan(t, dt, pool, "")
		bps := int64(40)
		if _, err := SetContract(ctx, pool, Contract{WorkspaceID: ws, PlatformFeeBPS: &bps, Reference: "B32.11"}, "operator@talyvor"); err != nil {
			t.Fatal(err)
		}
		if _, err := dt.SpendLXCMeta(ctx, ws, 10_000_000, "chat: metered usage", nil); err != nil {
			t.Fatal(err)
		}
		if rows := spendAndFee(t, pool, ws); len(rows) != 2 || rows[1].amount != -40_000 {
			t.Fatalf("rows = %+v, want a −40,000 platform_fee row at the contract's 40 bps", rows)
		}
	})

	t.Run("a balance that covers the call but not its fee is refused, and nothing is written", func(t *testing.T) {
		ws := onPlan(t, dt, pool, "team")
		if _, err := dt.SpendLXCMeta(ctx, ws, 99_800_000, "chat: metered usage", nil); !errors.Is(err, economy.ErrInsufficientLXC) {
			t.Fatalf("a 99.8 LXC call on 100 LXC at 3%% = %v, want ErrInsufficientLXC", err)
		}
		if rows := spendAndFee(t, pool, ws); len(rows) != 0 {
			t.Fatalf("a refused call wrote %+v", rows)
		}
	})
}

func TestB3211_AnAgentWith10Point2LXCOnTeamIsRefusedA10LXCCall(t *testing.T) {
	dt, pool := feeStore(t)
	ctx := context.Background()
	ws := onPlan(t, dt, pool, "team")
	key := "key-" + ws
	a, err := dt.CreateAgent(ctx, ws, "researcher", "user-"+ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := dt.AttachAgentKey(ctx, ws, a.ID, key); err != nil {
		t.Fatal(err)
	}
	if _, err := dt.FundAgent(ctx, ws, a.ID, 10_200_000); err != nil {
		t.Fatal(err)
	}
	meta := economy.AgentDebitMeta{RequestedModel: "gpt-4o", RequestID: "req-" + ws}
	if err := dt.ReserveLXCForAgent(ctx, key, ws, "res-1-"+ws, 10_000_000, meta); !errors.Is(err, economy.ErrSubBudgetExceeded) {
		t.Fatalf("a 10 LXC call with 10.2 LXC left on Team = %v, want ErrSubBudgetExceeded (with the fee it costs 10.3)", err)
	}
	var postings int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_postings WHERE workspace_id = $1 AND kind IN ('hold', 'platform_fee')`, ws).
		Scan(&postings); err != nil || postings != 0 {
		t.Fatalf("the refused call posted %d holds or fees (%v), want none", postings, err)
	}

	// 0.1 LXC more and the same call goes through, held at 10.3 and settled as 10 plus the 0.3 fee.
	if _, err := dt.FundAgent(ctx, ws, a.ID, 100_000); err != nil {
		t.Fatal(err)
	}
	if err := dt.ReserveLXCForAgent(ctx, key, ws, "res-2-"+ws, 10_000_000, meta); err != nil {
		t.Fatalf("a 10 LXC call with 10.3 LXC left: %v", err)
	}
	if settled, _, err := dt.SettleLXCReservation(ctx, "res-2-"+ws, 10_000_000, economy.AgentDebitMeta{ServedModel: "gpt-4o"}); err != nil || settled != 10_000_000 {
		t.Fatalf("settle = %d, %v; want 10,000,000", settled, err)
	}
	rows := spendAndFee(t, pool, ws)
	if len(rows) != 2 || rows[0].amount != -10_000_000 || rows[1].typ != "platform_fee" || rows[1].amount != -300_000 ||
		rows[0].xmin != rows[1].xmin || rows[1].requestID != "req-"+ws {
		t.Fatalf("rows = %+v, want −10,000,000 spend and −300,000 platform_fee in one transaction, naming the request", rows)
	}
	lines, err := dt.AgentStatement(ctx, ws, a.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) == 0 || lines[0].Kind != "platform_fee" || lines[0].AmountULXC != -300_000 ||
		lines[0].Label != "Platform fee 3%" || lines[0].BalanceAfterULXC != 0 {
		t.Fatalf("the statement's newest line = %+v, want the −300,000 platform fee, \"Platform fee 3%%\", leaving 0", lines[0])
	}
}
