package billing

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// B13.1, B32.13 — each plan's included usage D is computed from its price, and a subscriber who uses all
// of D leaves Talyvor 10% of their fee net of Stripe's fees, after upstream cost and pooled-answer royalties.

// netOfStripeULXC is F net of Stripe's fees in µLXC: what Talyvor keeps of a plan's price after the card's
// 2.9% + 30¢ and Stripe Billing's 0.7%.
func netOfStripeULXC(feeCents int64) int64 { return (964*feeCents - 30_000) * ulxcPerCent / 1000 }

// talyvorCostULXC is what Talyvor pays when a subscriber draws all of d at pooled share h: they consume
// T = d / (1 − 0.3h) of metered value, the unpooled (1 − h) of it is paid upstream, and the pooled h of it
// pays its contributors 0.35 × list.
func talyvorCostULXC(d int64, h float64) float64 {
	return float64(d) / (1 - pooledDiscount*h) * (1 - h + pooledRoyalty*h)
}

// resetIncludedUsage clears the monthly figures. token_events is append-only; rows from earlier runs
// drop out on their own, because newAllowanceService clears subscription_allowance and a request only
// counts while its workspace is inside an allowance period.
func resetIncludedUsage(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `DELETE FROM plan_included_usage`); err != nil {
		t.Fatal(err)
	}
}

// seedChat writes n subscriber chat requests for ws in the 30 days before month.
func seedChat(t *testing.T, pool *pgxpool.Pool, ws string, month time.Time, n int, source string, costUSD float64) {
	t.Helper()
	for i := 0; i < n; i++ {
		at := month.Add(-time.Duration(i+1) * 30 * time.Minute)
		if _, err := pool.Exec(context.Background(), `INSERT INTO token_events
			(workspace_id, provider, model, input_tokens, output_tokens, cost_usd, serve_source, auth_method, created_at)
			VALUES ($1, 'openai', 'gpt-4o', 1000, 1000, $2, $3, 'session_key', $4)`, ws, costUSD, source, at); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIncludedUsageULXC_AtNoPooling_IsNinetyPercentOfThePriceNetOfStripe(t *testing.T) {
	for fee, want := range map[int64]int64{2000: 170_820_000, 10000: 864_900_000, 20000: 1_732_500_000} {
		if got := IncludedUsageULXC(fee, 0); got != want {
			t.Errorf("IncludedUsageULXC(%d, 0) = %d µLXC, want %d", fee, got, want)
		}
	}
}

func TestIncludedUsage_EachPlanGrantsDAndUsingAllOfItLeavesTalyvorTenPercent(t *testing.T) {
	svc, pool := newAllowanceService(t)
	resetIncludedUsage(t, pool)
	ctx := context.Background()
	now := time.Now()
	start, end := period(now)
	for _, plan := range []struct {
		name     string
		feeCents int64
		wantD    int64 // 90% of F net of Stripe's fees at h = 0, exactly
	}{
		{"plus", 2000, 170_820_000},   // $17.082
		{"pro", 10000, 864_900_000},   // $86.49
		{"max", 20000, 1_732_500_000}, // $173.25
	} {
		t.Run(plan.name, func(t *testing.T) {
			ws := fmt.Sprintf("ws-plan-%s-%d", plan.name, now.UnixNano())
			seedWS(t, pool, ws)
			if _, err := svc.grantPeriod(ctx, ws, "sub_"+ws, start, end, plan.feeCents); err != nil {
				t.Fatalf("grant: %v", err)
			}
			a, err := svc.CurrentAllowance(ctx, ws, now)
			if err != nil || a == nil {
				t.Fatalf("allowance = %+v, %v", a, err)
			}
			if a.GrantedULXC != plan.wantD {
				t.Fatalf("granted D = %d µLXC, want %d", a.GrantedULXC, plan.wantD)
			}
			// The subscriber uses ALL of D — and not a µLXC more: past it they are on prepaid.
			covered, inPeriod, err := svc.Draw(ctx, ws, a.GrantedULXC+1_000_000, now)
			if err != nil || !inPeriod || covered != a.GrantedULXC {
				t.Fatalf("drawing past D covered %d (in period %v, %v), want exactly D %d", covered, inPeriod, err, a.GrantedULXC)
			}
			// At h = 0 every µLXC of D is provider cost with no markup: exactly 90% of the fee net of Stripe's fees.
			if net := netOfStripeULXC(plan.feeCents); covered*10 != net*9 {
				t.Errorf("using all of D costs Talyvor %d µLXC; want exactly 90%% of the fee net of Stripe's fees %d", covered, net)
			}
		})
	}
}

func TestIncludedUsage_RecomputedFromMeasuredPooledShare_CappedAt25PercentAMonth(t *testing.T) {
	svc, pool := newAllowanceService(t)
	resetIncludedUsage(t, pool)
	ctx := context.Background()
	month := monthOf(time.Now())
	ws := fmt.Sprintf("ws-plan-h-%d", time.Now().UnixNano())
	seedWS(t, pool, ws)
	// A subscriber throughout the measured window (fee unknown ⇒ the configured grant, which is fine here).
	if _, err := svc.grantPeriod(ctx, ws, "sub_"+ws, month.AddDate(0, 0, -35), month.Add(time.Hour), 0); err != nil {
		t.Fatalf("grant window period: %v", err)
	}
	// 1,200 chat requests in the 30 days before the 1st: half upstream, half pooled, equal metered value.
	listUSD := 1000*2.5e-6 + 1000*10e-6 // gpt-4o, 1k in + 1k out
	seedChat(t, pool, ws, month, 600, "upstream", listUSD)
	seedChat(t, pool, ws, month, 600, "cache_hit_pooled", 0)

	h, n, err := svc.MeasurePooledShare(ctx, month)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1200 || h <= 0 || h >= 0.5 || math.Abs(h-wilsonLower(0.5, 1200)) > 1e-12 {
		t.Fatalf("h = %v over %d requests; want the Wilson lower bound %v (< the 0.5 point estimate) over 1200", h, n, wilsonLower(0.5, 1200))
	}

	// Last month's figure was $16 → D may rise to 125% of it this month, however high h says it could go.
	lastMonth := int64(160_000_000)
	if _, err := pool.Exec(ctx, `INSERT INTO plan_included_usage (month, fee_usd_cents, pooled_share, window_requests, included_ulxc)
		VALUES ($1, 2000, 0, 0, $2)`, month.AddDate(0, -1, 0), lastMonth); err != nil {
		t.Fatal(err)
	}
	d, err := svc.includedUsage(ctx, 2000, month.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if formula := IncludedUsageULXC(2000, h); d != 200_000_000 || formula <= d {
		t.Errorf("D = %d, want 200000000 (125%% of last month's %d; the formula alone gives %d)", d, lastMonth, formula)
	}
	var stored float64
	if err := pool.QueryRow(ctx, `SELECT pooled_share FROM plan_included_usage WHERE month = $1 AND fee_usd_cents = 2000`, month).Scan(&stored); err != nil || stored != h {
		t.Errorf("stored pooled_share = %v (%v), want %v", stored, err, h)
	}
	if cost, most := talyvorCostULXC(d, h), float64(netOfStripeULXC(2000))*0.9; cost > most {
		t.Errorf("at h = %.4f, using all of D = %d costs Talyvor %.0f µLXC, more than 90%% of the fee net of Stripe's fees %.0f", h, d, cost, most)
	}

	// Below 1,000 requests h is not measured: 999 all-pooled requests, three months back, still give 0.
	earlier := month.AddDate(0, -3, 0)
	ws2 := ws + "-floor"
	seedWS(t, pool, ws2)
	if _, err := svc.grantPeriod(ctx, ws2, "sub_"+ws2, earlier.AddDate(0, 0, -35), earlier.Add(time.Hour), 0); err != nil {
		t.Fatalf("grant floor period: %v", err)
	}
	seedChat(t, pool, ws2, earlier, 999, "cache_hit_pooled", 0)
	if h, n, err := svc.MeasurePooledShare(ctx, earlier); err != nil || n != 999 || h != 0 {
		t.Errorf("below the floor: h = %v over %d (%v), want 0 over 999", h, n, err)
	}
}

// B32.13 — at h = 0.5 a subscriber who draws the whole Plus allowance, half its metered value answered
// upstream and half from the pool, costs Talyvor upstream plus royalties exactly 90% of $20 net of Stripe's
// fees: never more, and short of it by less than one µLXC (D is rounded down).
func TestIncludedUsage_AWholePlusAllowanceAtHalfPooled_CostsTalyvorNinetyPercentOfTheNetFee(t *testing.T) {
	svc, pool := newAllowanceService(t)
	resetIncludedUsage(t, pool)
	ctx := context.Background()
	month := monthOf(time.Now())
	start, end := month.Add(time.Hour), month.AddDate(0, 1, 0)
	at := start.Add(time.Hour)
	if _, err := pool.Exec(ctx, `INSERT INTO plan_included_usage (month, fee_usd_cents, pooled_share, window_requests, included_ulxc)
		VALUES ($1, 2000, 0.5, 1200, $2)`, month, IncludedUsageULXC(2000, 0.5)); err != nil {
		t.Fatal(err)
	}
	ws := fmt.Sprintf("ws-plan-half-%d", time.Now().UnixNano())
	seedWS(t, pool, ws)
	if _, err := svc.grantPeriod(ctx, ws, "sub_"+ws, start, end, 2000); err != nil {
		t.Fatalf("grant: %v", err)
	}
	a, err := svc.CurrentAllowance(ctx, ws, at)
	if err != nil || a == nil || a.GrantedULXC != 215_106_666 {
		t.Fatalf("allowance = %+v, %v; want D = 215106666 µLXC at h = 0.5", a, err)
	}
	// Equal metered value T/2 each way: upstream answers draw T/2, pooled answers 0.7 × T/2 — 10/17 and 7/17 of D,
	// split to the nearest µLXC.
	upstream, _, err := svc.Draw(ctx, ws, (a.GrantedULXC*10+8)/17, at)
	if err != nil {
		t.Fatal(err)
	}
	pooled, _, err := svc.Draw(ctx, ws, a.GrantedULXC, at)
	if err != nil {
		t.Fatal(err)
	}
	a, err = svc.CurrentAllowance(ctx, ws, at)
	if err != nil || a == nil || a.ConsumedULXC != a.GrantedULXC || upstream+pooled != a.GrantedULXC {
		t.Fatalf("after drawing %d + %d the allowance is %+v, %v; want all of D consumed", upstream, pooled, a, err)
	}
	// Upstream answers cost their list price; a pooled answer pays its contributor 0.35 of list, half its 0.7 draw.
	// Doubled to stay in whole µLXC: 2 × cost = 2 × upstream + pooled.
	twiceCost, twiceTarget := 2*upstream+pooled, 2*netOfStripeULXC(2000)*9/10
	if twiceCost > twiceTarget || twiceTarget-twiceCost >= 2 {
		t.Errorf("the whole allowance costs Talyvor %.1f µLXC (upstream %d + royalties %.1f); want 90%% of the net fee, %d, to the µLXC, rounded down",
			float64(twiceCost)/2, upstream, float64(pooled)/2, twiceTarget/2)
	}
}
