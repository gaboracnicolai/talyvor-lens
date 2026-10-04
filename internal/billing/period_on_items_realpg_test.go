package billing

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// B17.42 — a subscription event in Stripe's 2025-03-31 shape names its period on its items only. Subscribing
// grants the period's allowance, and cancelling keeps the day the plan ends, so Lens's read names it.
func TestSubscription_PeriodOnItems_GrantsAndKeepsTheEndThroughCancel(t *testing.T) {
	svc, pool := newAllowanceService(t)
	svc = svc.WithSubscriptions(&fakeSubStripe{}, "price_test_model2")
	seedWS(t, pool, "ws-b1742")
	now := time.Now()
	start, end := now.Add(-time.Hour).Truncate(time.Second), now.Add(30*24*time.Hour).Truncate(time.Second)
	basil := func(cancel bool) map[string]any {
		obj := subObj("sub_b1742", "ws-b1742", "cus_b1742", "price_test_model2", "active", end, cancel)
		delete(obj, "current_period_end")
		obj["items"] = map[string]any{"data": []any{map[string]any{
			"price":                map[string]any{"id": "price_test_model2"},
			"current_period_start": start.Unix(),
			"current_period_end":   end.Unix(),
		}}}
		return obj
	}

	body, sig := signedAt(testWebhookSecret, "evt_b1742_created", "customer.subscription.created", now, basil(false))
	if c := postEvent(svc, body, sig); c != http.StatusOK {
		t.Fatalf("created webhook = %d", c)
	}
	a, err := svc.CurrentAllowance(context.Background(), "ws-b1742", now)
	if err != nil {
		t.Fatalf("CurrentAllowance: %v", err)
	}
	if a == nil || a.GrantedULXC != testGrant {
		t.Fatalf("allowance = %+v, want %d µLXC granted for the period on the item", a, testGrant)
	}

	body, sig = signedAt(testWebhookSecret, "evt_b1742_cancel", "customer.subscription.updated", now.Add(time.Second), basil(true))
	if c := postEvent(svc, body, sig); c != http.StatusOK {
		t.Fatalf("updated webhook = %d", c)
	}
	st, err := svc.GetSubscription(context.Background(), "ws-b1742")
	if err != nil {
		t.Fatalf("GetSubscription: %v", err)
	}
	if !st.CancelAtPeriodEnd || st.CurrentPeriodEnd == nil || !st.CurrentPeriodEnd.Equal(end.UTC()) {
		t.Errorf("after cancel Lens reads cancel_at_period_end %v, period end %v; want true and %v",
			st.CancelAtPeriodEnd, st.CurrentPeriodEnd, end.UTC())
	}
}
