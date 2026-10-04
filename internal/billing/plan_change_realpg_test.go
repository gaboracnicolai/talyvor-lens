package billing

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	stripe "github.com/stripe/stripe-go/v81"
)

// planSub is a subscription object as Stripe sends it: one item on priceID at feeCents a month, USD.
func planSub(subID, wsID, priceID string, feeCents int64, start, end time.Time) map[string]any {
	return map[string]any{
		"id": subID, "object": "subscription", "status": "active", "livemode": false,
		"customer":             map[string]any{"id": "cus_plan"},
		"cancel_at_period_end": false,
		"current_period_start": start.Unix(),
		"current_period_end":   end.Unix(),
		"metadata":             map[string]string{"workspace_id": wsID},
		"items": map[string]any{"object": "list", "data": []any{map[string]any{
			"id": "si_plan", "object": "subscription_item", "quantity": 1,
			"price": map[string]any{"id": priceID, "object": "price", "currency": "usd", "unit_amount": feeCents},
		}}},
	}
}

// stripeTestAPI answers the two Stripe calls a plan change makes — read the subscription, update it — and
// records the update exactly as Stripe would receive it.
type stripeTestAPI struct {
	mu     sync.Mutex
	sub    map[string]any
	update url.Values
}

func (s *stripeTestAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Method == http.MethodPost {
		body, _ := io.ReadAll(r.Body)
		s.update, _ = url.ParseQuery(string(body))
		item := s.sub["items"].(map[string]any)["data"].([]any)[0].(map[string]any)
		item["price"] = map[string]any{"id": s.update.Get("items[0][price]"), "object": "price", "currency": "usd", "unit_amount": 5000}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.sub)
}

// B18.14 — a test-mode subscriber moves from Plus to Pro with proration. The change goes to Stripe as a
// prorated price swap on the subscription's item; Stripe's customer.subscription.updated then records the
// new price and moves this period's allowance by the Pro-minus-Plus included usage for the share of the
// period left — on the ledger, once, however often the event is delivered.
func TestSubscription_PlanChange_PlusToPro_ProratesAndMovesTheAllowance(t *testing.T) {
	svc, pool, _ := newSubService(t)
	ctx := context.Background()
	for _, tbl := range []string{"subscription_plan_changes", "subscription_allowance"} {
		if _, err := pool.Exec(ctx, "DELETE FROM "+tbl); err != nil {
			t.Fatal(err)
		}
	}
	const ws, subID, secret = "ws-plan-change", "sub_plan_change", testWebhookSecret
	// One clock for the whole test: the proration is priced from the update event's own time, and a second
	// of drift between two time.Now reads moves the allowance by ~109 µLXC (B27.14).
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Add(-10 * 24 * time.Hour)
	end := start.Add(30 * 24 * time.Hour)

	fake := &stripeTestAPI{sub: planSub(subID, ws, "price_plus", 2000, start, end)}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	prevBackend, prevKey := stripe.GetBackend(stripe.APIBackend), stripe.Key
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{URL: stripe.String(srv.URL)}))
	t.Cleanup(func() { stripe.SetBackend(stripe.APIBackend, prevBackend); stripe.Key = prevKey })
	svc.WithPlans(NewLiveStripe("sk_test_plan_change", "", ""), map[string]string{"plus": "price_plus", "pro": "price_pro"})

	// Subscribed on Plus: the period's allowance is Plus's.
	body, sig := signedAt(secret, "evt_plan_created", "customer.subscription.created", start, planSub(subID, ws, "price_plus", 2000, start, end))
	if code := postEvent(svc, body, sig); code != http.StatusOK {
		t.Fatalf("created = %d", code)
	}
	plus, _ := svc.includedUsage(ctx, 2000, start)
	pro, _ := svc.includedUsage(ctx, 5000, start)
	if a, err := svc.CurrentAllowance(ctx, ws, time.Now()); err != nil || a == nil || a.GrantedULXC != plus {
		t.Fatalf("Plus allowance = %+v, %v; want %d", a, err, plus)
	}

	// Change to Pro: Stripe is asked for a prorated swap of the item's price.
	if _, err := svc.ChangePlan(ctx, ws, "pro"); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	sent := fake.update
	fake.mu.Unlock()
	if sent.Get("items[0][id]") != "si_plan" || sent.Get("items[0][price]") != "price_pro" || sent.Get("proration_behavior") != "create_prorations" {
		t.Fatalf("Stripe was sent %v; want item si_plan moved to price_pro with create_prorations", sent)
	}

	// Stripe's update event records Pro and moves the allowance — once, though it is delivered twice.
	body, sig = signedAt(secret, "evt_plan_updated", "customer.subscription.updated", now, planSub(subID, ws, "price_pro", 5000, start, end))
	for range 2 {
		if code := postEvent(svc, body, sig); code != http.StatusOK {
			t.Fatalf("updated = %d", code)
		}
	}
	var price string
	var granted, fee, before, after, changes int64
	var left float64
	if err := pool.QueryRow(ctx, `SELECT price_id FROM subscriptions WHERE stripe_subscription_id = $1`, subID).Scan(&price); err != nil || price != "price_pro" {
		t.Fatalf("subscription price = %q, %v; want price_pro", price, err)
	}
	if err := pool.QueryRow(ctx, `SELECT granted_ulxc, fee_usd_cents FROM subscription_allowance WHERE stripe_subscription_id = $1`, subID).Scan(&granted, &fee); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*), max(granted_before_ulxc), max(granted_after_ulxc), max(remaining_fraction)
		FROM subscription_plan_changes WHERE stripe_subscription_id = $1`, subID).Scan(&changes, &before, &after, &left); err != nil {
		t.Fatal(err)
	}
	want := plus + int64(float64(pro-plus)*(20.0/30.0)+0.5)
	if changes != 1 || before != plus || after != granted || fee != 5000 || left < 0.66 || left > 0.67 || after < want-1 || after > want+1 || after <= plus {
		t.Errorf("plan change: %d rows, allowance %d → %d (row %d at fee %d, %.3f of the period left); want 1 row, %d → about %d at 5000",
			changes, before, after, granted, fee, left, plus, want)
	}
}
