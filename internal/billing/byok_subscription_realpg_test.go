package billing

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// B27.26 — a BYOK subscription is recorded as BYOK and its period grants no token allowance: the $199 is the
// platform fee, and the workspace's requests are paid on its own provider keys. The control is a subscription
// to another Price on the same Service, which does grant — so the grant path is live in this setup.
func TestB2726_BYOKSubscriptionIsRecordedAndGrantsNoAllowance(t *testing.T) {
	svc, pool, _ := newSubService(t)
	svc = svc.WithAllowance(5_000_000)
	ctx := context.Background()
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	end := start.Add(30 * 24 * time.Hour)

	for _, tc := range []struct {
		ws, sub, lookupKey string
		byok               bool
	}{
		{"ws-b2726-byok", "sub_b2726_byok", BYOKLookupKey, true},
		{"ws-b2726-plus", "sub_b2726_plus", "talyvor_plus_monthly", false},
	} {
		seedWS(t, pool, tc.ws)
		if _, err := pool.Exec(ctx, `DELETE FROM subscription_allowance WHERE workspace_id = $1`, tc.ws); err != nil {
			t.Fatal(err)
		}
		obj := subObj(tc.sub, tc.ws, "cus_"+tc.ws, "price_"+tc.lookupKey, "active", end, false)
		obj["current_period_start"] = start.Unix()
		obj["items"] = map[string]any{"data": []any{map[string]any{
			"price": map[string]any{"id": "price_" + tc.lookupKey, "lookup_key": tc.lookupKey}}}}
		body, sig := signedAt(testWebhookSecret, "evt_"+tc.sub, "customer.subscription.created", time.Now(), obj)
		if code := postEvent(svc, body, sig); code != http.StatusOK {
			t.Fatalf("%s: webhook code = %d", tc.ws, code)
		}

		var byok bool
		if err := pool.QueryRow(ctx, `SELECT byok FROM subscriptions WHERE stripe_subscription_id = $1`, tc.sub).Scan(&byok); err != nil {
			t.Fatalf("%s: no subscriptions row: %v", tc.ws, err)
		}
		var grants int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM subscription_allowance WHERE workspace_id = $1`, tc.ws).Scan(&grants); err != nil {
			t.Fatal(err)
		}
		st, err := svc.GetSubscription(ctx, tc.ws)
		if err != nil {
			t.Fatal(err)
		}
		wantGrants := 1
		if tc.byok {
			wantGrants = 0
		}
		if byok != tc.byok || st.BYOK != tc.byok || !st.Subscribed || grants != wantGrants {
			t.Errorf("%s: subscriptions.byok %v, read model byok %v subscribed %v, %d allowance rows — want byok %v, subscribed, %d rows",
				tc.ws, byok, st.BYOK, st.Subscribed, grants, tc.byok, wantGrants)
		}
	}
}
