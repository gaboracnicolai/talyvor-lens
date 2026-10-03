package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	stripe "github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/webhook"

	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/economy"
)

// b1721Stripe is a Stripe test-mode account holding B13.1's three plan Prices under their lookup keys, and the
// subscription a Plus checkout makes. It records the key and form of every call.
type b1721Stripe struct {
	mu        sync.Mutex
	calls     []stripeCall
	periodEnd int64
}

func (s *b1721Stripe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	if r.Method == http.MethodGet {
		form = r.URL.Query()
	}
	s.mu.Lock()
	s.calls = append(s.calls, stripeCall{path: r.URL.Path, key: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), form: form})
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v1/prices":
		var data []map[string]any
		for plan, cents := range map[string]int{"plus": 2000, "pro": 10000, "max": 20000} {
			data = append(data, map[string]any{"id": "price_test_" + plan, "object": "price", "active": true,
				"lookup_key": "talyvor_" + plan + "_monthly", "currency": "usd", "unit_amount": cents})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "url": "/v1/prices", "has_more": false, "data": data})
	case "/v1/customers":
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "cus_b1721", "object": "customer"})
	case "/v1/checkout/sessions":
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "cs_test_b1721", "object": "checkout.session", "url": "https://checkout.stripe.com/c/pay/cs_test_b1721"})
	case "/v1/subscriptions/sub_b1721":
		_ = json.NewEncoder(w).Encode(s.subscription())
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"not in this fake"}}`)
	}
}

// subscription is the Plus subscription the test user's checkout made, as Stripe serialises it.
func (s *b1721Stripe) subscription() map[string]any {
	return map[string]any{"id": "sub_b1721", "object": "subscription", "status": "active", "customer": "cus_b1721",
		"current_period_start": s.periodEnd - 30*86400, "current_period_end": s.periodEnd, "cancel_at_period_end": false,
		"metadata": map[string]string{"workspace_id": "s-b1721-tester"},
		"items": map[string]any{"object": "list", "data": []map[string]any{{"id": "si_b1721", "object": "subscription_item", "quantity": 1,
			"price": map[string]any{"id": "price_test_plus", "object": "price", "currency": "usd", "unit_amount": 2000}}}}}
}

func (s *b1721Stripe) paths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var p []string
	for _, c := range s.calls {
		p = append(p, c.path)
	}
	return p
}

// B17.21 — ON A LENS WHOSE OWN STRIPE KEY IS TEST MODE AND WHOSE ENV NAMES NO PLAN (production's wiring), A TEST
// USER SUBSCRIBES TO PLUS AND LENS GRANTS THE PERIOD'S ALLOWANCE.
//
// The plans are the Prices the Stripe account holds under B13.1's lookup keys; the test user pays through the
// test-mode main Service, there being no other; and the completed checkout alone records the subscription and
// grants the allowance, on an endpoint that sends Stripe's checkout events and no customer.subscription.*.
func TestB1721_ATestUserSubscribesToPlusOnATestModeLensWithNoPlanConfigured(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const testWS, secret, key = "s-b1721-tester", "whsec_b1721", "sk_test_b1721"
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, true)`, testWS); err != nil {
		t.Fatal(err)
	}
	fake := &b1721Stripe{periodEnd: time.Now().Add(29 * 24 * time.Hour).Unix()}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	prevBackend, prevKey := stripe.GetBackend(stripe.APIBackend), stripe.Key
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{URL: stripe.String(srv.URL)}))
	t.Cleanup(func() { stripe.SetBackend(stripe.APIBackend, prevBackend); stripe.Key = prevKey })

	// A live key sells only what env configures: Stripe is not even asked.
	if plans := sellablePlans(ctx, nil, "sk_live_b1721", billing.NewTestModeStripe("sk_live_b1721", "", ""), "LENS_BILLING_SUBSCRIPTION_PLANS"); len(plans) != 0 || len(fake.paths()) != 0 {
		t.Fatalf("a live key with no plan configured sells %v after Stripe calls %v; want nothing, and no call", plans, fake.paths())
	}

	liveStripe := billing.NewLiveStripe(key, "", "")
	plans := sellablePlans(ctx, nil, key, liveStripe, "LENS_BILLING_SUBSCRIPTION_PLANS")
	if plans["plus"] != "price_test_plus" || plans["pro"] != "price_test_pro" || plans["max"] != "price_test_max" || len(plans) != 3 {
		t.Fatalf("the test-mode account's plans = %v; want plus, pro and max by their lookup keys", plans)
	}
	svc := billing.New(pool, economy.NewDualTokenStore(nil, pool, nil), liveStripe, secret).WithPlans(liveStripe, plans)
	isTest := func(ws string) bool { return ws == testWS }
	b := newBillingRouter(svc, nil, isTest, key, "", "")
	if !b.sellsSubscriptions() {
		t.Fatal("the subscription routes would not be registered")
	}
	r := chi.NewRouter()
	r.Post("/v1/workspaces/{wsID}/billing/subscribe", b.onSubscriptions(true, newSubscribeHandler))
	r.Post("/v1/billing/webhook", svc.HandleWebhook)

	// Plans → Choose Plus: Lens opens a Stripe test-mode checkout for the Plus Price.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+testWS+"/billing/subscribe", strings.NewReader(`{"plan":"plus"}`)))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "checkout.stripe.com") {
		t.Fatalf("the test user's Choose Plus = %d %s; want Stripe's checkout URL", w.Code, w.Body.String())
	}
	var checkout stripeCall
	for _, c := range fake.calls {
		if c.path == "/v1/checkout/sessions" {
			checkout = c
		}
	}
	if checkout.key != key || checkout.form.Get("line_items[0][price]") != "price_test_plus" || checkout.form.Get("mode") != "subscription" {
		t.Fatalf("the checkout went to Stripe with key %q, price %q, mode %q; want %s, price_test_plus, subscription",
			checkout.key, checkout.form.Get("line_items[0][price]"), checkout.form.Get("mode"), key)
	}

	// Paid with card 4242: Stripe sends checkout.session.completed — and, on this endpoint, nothing else.
	deliver := func(id, eventType string, created int64, object map[string]any) {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"id": id, "object": "event", "type": eventType, "created": created,
			"livemode": false, "data": map[string]any{"object": object}})
		signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: payload, Secret: secret})
		req := httptest.NewRequest(http.MethodPost, "/v1/billing/webhook", strings.NewReader(string(payload)))
		req.Header.Set("Stripe-Signature", signed.Header)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", eventType, w.Code, w.Body.String())
		}
	}
	now := time.Now().Unix()
	deliver("evt_b1721_completed", "checkout.session.completed", now, map[string]any{"id": "cs_test_b1721", "object": "checkout.session",
		"mode": "subscription", "payment_status": "paid", "status": "complete", "subscription": "sub_b1721",
		"metadata": map[string]string{"workspace_id": testWS}})

	var status, price string
	if err := pool.QueryRow(ctx, `SELECT status, price_id FROM subscriptions WHERE workspace_id = $1`, testWS).Scan(&status, &price); err != nil ||
		status != "active" || price != "price_test_plus" {
		t.Fatalf("the test user's subscription = %q on %q (%v); want active on price_test_plus", status, price, err)
	}
	grants := func() (n int, granted, fee int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(granted_ulxc), 0), COALESCE(max(fee_usd_cents), 0)
			FROM subscription_allowance WHERE workspace_id = $1`, testWS).Scan(&n, &granted, &fee); err != nil {
			t.Fatal(err)
		}
		return
	}
	n, granted, fee := grants()
	if n != 1 || fee != 2000 || granted <= 0 {
		t.Fatalf("the test user's allowance: %d grant(s), %d µLXC for %d cents; want one, for Plus's 2000 cents", n, granted, fee)
	}
	sum, err := svc.Summary(ctx, testWS, time.Now())
	if err != nil || sum.Allowance == nil || sum.Allowance.FeeUSDCents != 2000 || sum.Allowance.RemainingULXC != granted {
		t.Fatalf("GET …/billing/allowance would read %+v (%v); want the Plus period, all %d µLXC left", sum, err, granted)
	}

	// On an endpoint that does send customer.subscription.created, it is older than the checkout: refused as stale.
	deliver("evt_b1721_created", "customer.subscription.created", now-5, fake.subscription())
	if n2, granted2, _ := grants(); n2 != 1 || granted2 != granted {
		t.Fatalf("after customer.subscription.created: %d grant(s) of %d µLXC; want the one grant of %d unchanged", n2, granted2, granted)
	}
}
