package billing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// MODEL 2 STEP 2 — the allowance ledger, on real Postgres. W4.6.1.
//
// ⚠ THE PROPERTY UNDER TEST IS A BOUND, NOT A FEATURE. The item's claim is that the
// worst case per subscriber is EXACTLY D, which is what lets the product be priced
// instead of hoped for. A bound is only worth the adversarial cases run against it,
// so most of this file is attempts to exceed D: overspending in one call, in many
// calls, and in many calls AT ONCE.

const testGrant int64 = 1_000_000 // D for these tests, in µLXC (= 1 LXC). Not a price.

func resetAllowance(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), "DELETE FROM subscription_allowance"); err != nil {
		t.Fatalf("reset subscription_allowance: %v", err)
	}
}

func newAllowanceService(t *testing.T) (*Service, *pgxpool.Pool) {
	t.Helper()
	svc, pool, _ := newBillingService(t)
	resetSubs(t, pool)
	resetAllowance(t, pool)
	return svc.WithAllowance(testGrant), pool
}

func period(now time.Time) (time.Time, time.Time) {
	return now.Add(-24 * time.Hour), now.Add(29 * 24 * time.Hour)
}

func TestAllowance_GrantThenConsume_WithinD(t *testing.T) {
	svc, pool := newAllowanceService(t)
	seedWS(t, pool, "ws-al-1")
	now := time.Now()
	start, end := period(now)

	created, err := svc.Grant(context.Background(), "ws-al-1", "sub_al_1", start, end)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if !created {
		t.Fatal("Grant reported no row created")
	}

	covered, err := svc.Consume(context.Background(), "ws-al-1", 400_000, now)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if covered != 400_000 {
		t.Errorf("covered = %d, want 400000 — the allowance must cover a cost inside D in full", covered)
	}
	a, _ := svc.CurrentAllowance(context.Background(), "ws-al-1", now)
	if a == nil || a.RemainingULXC != testGrant-400_000 {
		t.Errorf("remaining = %+v, want %d", a, testGrant-400_000)
	}
}

// ⚠ THE HARD CAP, THE OBVIOUS WAY: one cost larger than the whole allowance.
func TestAllowance_HardCap_SingleOverspendIsClamped(t *testing.T) {
	svc, pool := newAllowanceService(t)
	seedWS(t, pool, "ws-al-2")
	now := time.Now()
	start, end := period(now)
	if _, err := svc.Grant(context.Background(), "ws-al-2", "sub_al_2", start, end); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	covered, err := svc.Consume(context.Background(), "ws-al-2", testGrant*10, now)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	// ⚠ CLAMPED, NOT REFUSED. Refusing would strand the allowance: every real request
	// costs more than the last sliver, so a refusing cap makes the tail unspendable.
	if covered != testGrant {
		t.Errorf("covered = %d, want exactly D (%d) — the cap must clamp, not refuse and not overflow",
			covered, testGrant)
	}
	a, _ := svc.CurrentAllowance(context.Background(), "ws-al-2", now)
	if a.ConsumedULXC != testGrant || a.RemainingULXC != 0 {
		t.Errorf("consumed = %d remaining = %d, want %d and 0", a.ConsumedULXC, a.RemainingULXC, testGrant)
	}

	// And once exhausted it stays exhausted — the next request is covered by nothing.
	covered, err = svc.Consume(context.Background(), "ws-al-2", 50_000, now)
	if err != nil {
		t.Fatalf("Consume after exhaustion: %v", err)
	}
	if covered != 0 {
		t.Errorf("covered = %d after exhaustion, want 0 — this is OVERAGE, which the item forbids", covered)
	}
}

// ⚠ THE HARD CAP, THE PATIENT WAY: many small costs that sum past D.
func TestAllowance_HardCap_ManySmallConsumesCannotExceedD(t *testing.T) {
	svc, pool := newAllowanceService(t)
	seedWS(t, pool, "ws-al-3")
	now := time.Now()
	start, end := period(now)
	if _, err := svc.Grant(context.Background(), "ws-al-3", "sub_al_3", start, end); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	var total int64
	for i := 0; i < 30; i++ {
		c, err := svc.Consume(context.Background(), "ws-al-3", 50_000, now) // 30 × 50k = 1.5D
		if err != nil {
			t.Fatalf("Consume %d: %v", i, err)
		}
		total += c
	}
	if total != testGrant {
		t.Errorf("total covered = %d, want exactly D (%d) — a bound that leaks over many small "+
			"draws is not a bound", total, testGrant)
	}
}

// ⚠ THE HARD CAP, THE ADVERSARIAL WAY: concurrent settles racing the same row.
//
// This is the case a SELECT-then-UPDATE loses. Both readers see the same remaining,
// both add their cost, and the sum exceeds D — or the DB CHECK rejects the loser and
// a legitimate settle fails. `LEAST` computed inside the UPDATE, under FOR UPDATE, is
// what makes the two serialise.
func TestAllowance_HardCap_ConcurrentConsumesCannotExceedD(t *testing.T) {
	svc, pool := newAllowanceService(t)
	seedWS(t, pool, "ws-al-4")
	now := time.Now()
	start, end := period(now)
	if _, err := svc.Grant(context.Background(), "ws-al-4", "sub_al_4", start, end); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	const workers = 16
	const each = 100_000 // 16 × 100k = 1.6D
	var (
		mu    sync.Mutex
		total int64
		errs  []error
		wg    sync.WaitGroup
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := svc.Consume(context.Background(), "ws-al-4", each, now)
			mu.Lock()
			defer mu.Unlock()
			total += c
			if err != nil {
				errs = append(errs, err)
			}
		}()
	}
	wg.Wait()

	for _, e := range errs {
		t.Errorf("a concurrent settle FAILED rather than being clamped: %v", e)
	}
	if total != testGrant {
		t.Errorf("total covered under concurrency = %d, want exactly D (%d)", total, testGrant)
	}
	a, _ := svc.CurrentAllowance(context.Background(), "ws-al-4", now)
	if a.ConsumedULXC != testGrant {
		t.Errorf("consumed = %d, want %d — the DB CHECK is the backstop and it must never be the "+
			"thing that noticed", a.ConsumedULXC, testGrant)
	}
}

// ⚠ ONE GRANT PER PERIOD: a redelivered renewal must not hand out 2D for one fee.
func TestAllowance_GrantIsIdempotentPerPeriod(t *testing.T) {
	svc, pool := newAllowanceService(t)
	seedWS(t, pool, "ws-al-5")
	now := time.Now()
	start, end := period(now)

	first, err := svc.Grant(context.Background(), "ws-al-5", "sub_al_5", start, end)
	if err != nil || !first {
		t.Fatalf("first Grant: created=%v err=%v", first, err)
	}
	second, err := svc.Grant(context.Background(), "ws-al-5", "sub_al_5", start, end)
	if err != nil {
		t.Fatalf("second Grant: %v", err)
	}
	if second {
		t.Error("the second grant for the same period created a row — one subscriber, one fee, 2D")
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM subscription_allowance WHERE stripe_subscription_id = $1`, "sub_al_5").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("allowance rows = %d, want 1", n)
	}
}

// A new period is a new grant — the allowance resets by ARRIVING, never by a reset.
func TestAllowance_NextPeriodIsANewGrant(t *testing.T) {
	svc, pool := newAllowanceService(t)
	seedWS(t, pool, "ws-al-6")
	now := time.Now()
	p1s, p1e := now.Add(-40*24*time.Hour), now.Add(-10*24*time.Hour)
	p2s, p2e := now.Add(-10*24*time.Hour), now.Add(20*24*time.Hour)

	if _, err := svc.Grant(context.Background(), "ws-al-6", "sub_al_6", p1s, p1e); err != nil {
		t.Fatalf("grant p1: %v", err)
	}
	if _, err := svc.Grant(context.Background(), "ws-al-6", "sub_al_6", p2s, p2e); err != nil {
		t.Fatalf("grant p2: %v", err)
	}
	// The CURRENT period is p2, and it is untouched by p1's consumption.
	if _, err := svc.Consume(context.Background(), "ws-al-6", 250_000, now); err != nil {
		t.Fatalf("consume: %v", err)
	}
	a, _ := svc.CurrentAllowance(context.Background(), "ws-al-6", now)
	if a == nil || !a.PeriodStart.Equal(p2s.UTC().Truncate(time.Microsecond)) && !a.PeriodStart.Round(time.Second).Equal(p2s.UTC().Round(time.Second)) {
		t.Fatalf("current period = %+v, want the one starting %v", a, p2s.UTC())
	}
	if a.ConsumedULXC != 250_000 {
		t.Errorf("consumed = %d, want 250000 in the CURRENT period only", a.ConsumedULXC)
	}
}

// ⚠ THE DEFAULT IS OFF, AND OFF MEANS COVERS NOTHING.
func TestAllowance_NotConfigured_GrantsNothingAndCoversNothing(t *testing.T) {
	svc, pool, _ := newBillingService(t) // NOT WithAllowance → D = 0
	resetSubs(t, pool)
	resetAllowance(t, pool)
	seedWS(t, pool, "ws-al-7")
	now := time.Now()
	start, end := period(now)

	if _, err := svc.Grant(context.Background(), "ws-al-7", "sub_al_7", start, end); !errors.Is(err, ErrNoAllowanceConfigured) {
		t.Fatalf("Grant with D=0: err = %v, want ErrNoAllowanceConfigured", err)
	}
	covered, err := svc.Consume(context.Background(), "ws-al-7", 500_000, now)
	if err != nil {
		t.Fatalf("Consume with no allowance: %v", err)
	}
	if covered != 0 {
		t.Errorf("covered = %d with no allowance configured, want 0", covered)
	}
}

// ⚠ A SUBSCRIPTION WEBHOOK GRANTS THE PERIOD — end to end, through HandleWebhook.
func TestAllowance_SubscriptionCreatedGrantsThePeriod(t *testing.T) {
	svc, pool := newAllowanceService(t)
	svc = svc.WithSubscriptions(&fakeSubStripe{}, "price_test_model2")
	seedWS(t, pool, "ws-al-8")
	now := time.Now()
	end := now.Add(30 * 24 * time.Hour)

	obj := subObj("sub_al_8", "ws-al-8", "cus_8", "price_test_model2", "active", end, false)
	obj["current_period_start"] = now.Add(-time.Hour).Unix()
	body, sig := signedAt(testWebhookSecret, "evt_al_8", "customer.subscription.created", now, obj)
	if c := postEvent(svc, body, sig); c != http.StatusOK {
		t.Fatalf("webhook = %d", c)
	}

	a, err := svc.CurrentAllowance(context.Background(), "ws-al-8", now)
	if err != nil {
		t.Fatalf("CurrentAllowance: %v", err)
	}
	if a == nil {
		t.Fatal("a subscription was created and no allowance was granted for its period")
	}
	if a.GrantedULXC != testGrant || a.RemainingULXC != testGrant {
		t.Errorf("granted = %d remaining = %d, want %d and %d", a.GrantedULXC, a.RemainingULXC, testGrant, testGrant)
	}
}

// ⚠ AND A STALE EVENT MUST NOT GRANT ONE. An out-of-order event that the state
// machine refused must not hand out an allowance through the back door.
func TestAllowance_StaleEventGrantsNothing(t *testing.T) {
	svc, pool := newAllowanceService(t)
	svc = svc.WithSubscriptions(&fakeSubStripe{}, "price_test_model2")
	seedWS(t, pool, "ws-al-9")
	now := time.Now()
	end := now.Add(30 * 24 * time.Hour)

	// Newer: cancelled. (Terminal, so nothing later may reopen it.)
	o1 := subObj("sub_al_9", "ws-al-9", "cus_9", "price_test_model2", "active", end, false)
	o1["current_period_start"] = now.Add(-time.Hour).Unix()
	b1, s1 := signedAt(testWebhookSecret, "evt_al_9_del", "customer.subscription.deleted", now, o1)
	postEvent(svc, b1, s1)

	// Older: active, for a period the subscriber no longer has.
	o2 := subObj("sub_al_9", "ws-al-9", "cus_9", "price_test_model2", "active", end, false)
	o2["current_period_start"] = now.Add(-time.Hour).Unix()
	b2, s2 := signedAt(testWebhookSecret, "evt_al_9_stale", "customer.subscription.updated",
		now.Add(-10*time.Minute), o2)
	postEvent(svc, b2, s2)

	a, _ := svc.CurrentAllowance(context.Background(), "ws-al-9", now)
	if a != nil {
		t.Errorf("a stale event granted an allowance: %+v — the out-of-order bug, wearing a "+
			"different hat and an expensive one", a)
	}
}

// B1.6 — the webhook records F, the amount the granted period was billed at, off the
// subscription's own price: "Your plan is $20" is read from Stripe, not from config.
func TestAllowance_GrantRecordsThePeriodFee(t *testing.T) {
	svc, pool := newAllowanceService(t)
	svc = svc.WithSubscriptions(&fakeSubStripe{}, "price_test_model2")
	seedWS(t, pool, "ws-al-fee")
	now := time.Now()

	obj := subObj("sub_al_fee", "ws-al-fee", "cus_fee", "price_test_model2", "active", now.Add(30*24*time.Hour), false)
	obj["current_period_start"] = now.Add(-time.Hour).Unix()
	obj["items"] = map[string]any{"data": []any{map[string]any{
		"quantity": 1,
		"price":    map[string]any{"id": "price_test_model2", "currency": "usd", "unit_amount": 2000},
	}}}
	body, sig := signedAt(testWebhookSecret, "evt_al_fee", "customer.subscription.created", now, obj)
	if c := postEvent(svc, body, sig); c != http.StatusOK {
		t.Fatalf("webhook = %d", c)
	}
	a, err := svc.CurrentAllowance(context.Background(), "ws-al-fee", now)
	if err != nil || a == nil {
		t.Fatalf("CurrentAllowance = %+v, %v", a, err)
	}
	if a.FeeUSDCents != 2000 {
		t.Errorf("fee_usd_cents = %d, want 2000 (the price's unit_amount)", a.FeeUSDCents)
	}
}

// B1.6 — THE CEILING. What a subscriber's answers earned counts back against their
// plan only up to the fee: earnings of $25 against a $20 plan earn back $20, not $25.
// Revoked mints are not earnings; held ones are, and are reported as still held.
func TestAllowance_Summary_EarnedBackNeverExceedsTheFee(t *testing.T) {
	svc, pool := newAllowanceService(t)
	ctx := context.Background()
	ws := fmt.Sprintf("ws-al-earn-%d", time.Now().UnixNano())
	seedWS(t, pool, ws)
	now := time.Now()
	start, end := period(now)
	if _, err := svc.grantPeriod(ctx, ws, "sub_"+ws, start, end, 2000); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.Consume(ctx, ws, 250_000, now); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	// µLENS at $0.10/LENS: 150 LENS = $15, 100 LENS = $10, 500 LENS revoked, and a
	// mint from before the period — none of the last two count.
	mint := func(table, status string, ulens int64, at time.Time) {
		t.Helper()
		var err error
		if table == "pool_royalty_mints" {
			_, err = pool.Exec(ctx, `INSERT INTO pool_royalty_mints
				(request_id, requester_workspace_id, contributor_workspace_id, layer, minted_amount, status, created_at)
				VALUES ($1, 'ws-other', $2, 'exact', $3, $4, $5)`,
				fmt.Sprintf("%s-%s-%d", ws, status, ulens), ws, ulens, status, at)
		} else {
			_, err = pool.Exec(ctx, `INSERT INTO distill_royalty_mints
				(request_id, contributor_workspace_id, requester_workspace_id, content_hash, avoided_cogs_usd, minted_amount, status, created_at)
				VALUES ($1, $2, 'ws-other', 'h', 0, $3, $4, $5)`,
				fmt.Sprintf("%s-d-%s-%d", ws, status, ulens), ws, ulens, status, at)
		}
		if err != nil {
			t.Fatalf("seed %s: %v", table, err)
		}
	}
	mint("pool_royalty_mints", "final", 150_000_000, now)
	mint("distill_royalty_mints", "held", 100_000_000, now)
	mint("pool_royalty_mints", "revoked", 500_000_000, now)
	mint("pool_royalty_mints", "final", 900_000_000, start.Add(-time.Hour))

	sum, err := svc.Summary(ctx, ws, now)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	// B13.1: a $20 period grants the plan's computed included usage, not the configured figure.
	d := IncludedUsageULXC(2000, 0)
	if sum.Allowance == nil || sum.Allowance.ConsumedULXC != 250_000 || sum.Allowance.RemainingULXC != d-250_000 {
		t.Fatalf("allowance = %+v, want 250000 used of %d", sum.Allowance, d)
	}
	if sum.EarnedULENS != 250_000_000 || sum.EarnedHeldULENS != 100_000_000 {
		t.Errorf("earned = %d (held %d) µLENS, want 250000000 (held 100000000)", sum.EarnedULENS, sum.EarnedHeldULENS)
	}
	if sum.EarnedUSDCents != 2500 {
		t.Errorf("earned_usd_cents = %d, want 2500", sum.EarnedUSDCents)
	}
	if sum.EarnedBackUSDCents != 2000 {
		t.Errorf("earned_back_usd_cents = %d, want 2000 — earnings counted against the plan must stop at the $20 fee", sum.EarnedBackUSDCents)
	}
}
