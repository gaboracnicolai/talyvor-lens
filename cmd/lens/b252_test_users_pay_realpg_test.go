package main

import (
	"context"
	"encoding/json"
	"fmt"
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

// stripeCall is one request Lens made to Stripe: which secret key it carried, and what it sent.
type stripeCall struct {
	path, key string
	form      url.Values
}

// stripeKeyRecorder is Stripe's API for customers and Checkout Sessions, recording the key of every call.
type stripeKeyRecorder struct {
	mu    sync.Mutex
	calls []stripeCall
}

func (s *stripeKeyRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	s.mu.Lock()
	s.calls = append(s.calls, stripeCall{path: r.URL.Path, key: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), form: form})
	n := len(s.calls)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v1/customers":
		_ = json.NewEncoder(w).Encode(map[string]any{"id": fmt.Sprintf("cus_b252_%d", n), "object": "customer"})
	case "/v1/checkout/sessions":
		id := fmt.Sprintf("cs_b252_%d", n)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "object": "checkout.session", "url": "https://checkout.stripe.com/c/pay/" + id})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"not in this fake"}}`)
	}
}

// last returns the latest call to path.
func (s *stripeKeyRecorder) last(t *testing.T, path string) stripeCall {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.calls) - 1; i >= 0; i-- {
		if s.calls[i].path == path {
			return s.calls[i]
		}
	}
	t.Fatalf("Lens never called Stripe's %s", path)
	return stripeCall{}
}

func (s *stripeKeyRecorder) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c.path == path {
			n++
		}
	}
	return n
}

// B25.2 — A TEST USER TOPS UP AND SUBSCRIBES WITH TEST CARD 4242, THROUGH STRIPE TEST MODE, AND A REAL USER'S
// CHECKOUT IS UNCHANGED.
//
// Wired as main.go wires it once the live key is live: the live Service with sk_live_ and the test workspaces'
// Service with sk_test_, its own webhook secret and its own plans. Stripe is a recorder of which key each call
// carried; its webhook events are signed as Stripe signs them. Every assertion that money moved is on a row.
func TestB252_ATestUserPaysInStripeTestModeAndARealUsersCheckoutIsUnchanged(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const (
		testWS, realWS         = "s-b252-tester", "u-b252-real"
		liveSecret, testSecret = "whsec_b252_live", "whsec_b252_test"
	)
	for _, ws := range []struct {
		id        string
		synthetic bool
	}{{testWS, true}, {realWS, false}} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, $2)`,
			ws.id, ws.synthetic); err != nil {
			t.Fatal(err)
		}
	}

	fake := &stripeKeyRecorder{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	prevBackend, prevKey := stripe.GetBackend(stripe.APIBackend), stripe.Key
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{URL: stripe.String(srv.URL)}))
	t.Cleanup(func() { stripe.SetBackend(stripe.APIBackend, prevBackend); stripe.Key = prevKey })

	credits := economy.NewDualTokenStore(nil, pool, nil)
	liveStripe := billing.NewLiveStripe("sk_live_b252", "", "")
	live := billing.New(pool, credits, liveStripe, liveSecret).WithPlans(liveStripe, map[string]string{"plus": "price_live_plus"})
	testStripe := billing.NewTestModeStripe("sk_test_b252", "", "")
	test := billing.New(pool, credits, testStripe, testSecret).ForTestWorkspaces(true).
		WithPlans(testStripe, map[string]string{"plus": "price_test_plus"})
	live = live.ForTestWorkspaces(false)
	isTest := func(ws string) bool {
		var s bool
		_ = pool.QueryRow(ctx, `SELECT synthetic FROM workspaces WHERE id = $1`, ws).Scan(&s)
		return s
	}
	route := func(test *billing.Service) http.Handler {
		b := newBillingRouter(live, test, isTest, "sk_live_b252", "sk_test_b252", testSecret)
		if test == nil {
			b = newBillingRouter(live, nil, isTest, "sk_live_b252", "", "")
		}
		r := chi.NewRouter()
		r.Post("/v1/workspaces/{wsID}/billing/checkout", newCheckoutHandler(b))
		r.Post("/v1/workspaces/{wsID}/billing/subscribe", b.onSubscriptions(true, newSubscribeHandler))
		r.Post("/v1/billing/webhook", live.HandleWebhook)
		if test != nil {
			r.Post("/v1/billing/webhook/test", test.HandleWebhook)
		}
		return r
	}
	r := route(test)
	call := func(h http.Handler, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		return w
	}
	evt := 0
	deliver := func(path, secret, eventType string, object map[string]any) {
		t.Helper()
		evt++
		payload, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("evt_b252_%d_%d", time.Now().UnixNano(), evt), "object": "event",
			"type": eventType, "created": time.Now().Unix(), "livemode": secret == liveSecret, "data": map[string]any{"object": object}})
		signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: payload, Secret: secret})
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(payload)))
		req.Header.Set("Stripe-Signature", signed.Header)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s = %d %s", path, eventType, w.Code, w.Body.String())
		}
	}
	// paid is the Checkout Session Stripe completes once the card is charged, built from what Lens sent it.
	paid := func(c stripeCall, sessionID string) map[string]any {
		return map[string]any{"id": sessionID, "object": "checkout.session", "mode": "payment", "payment_status": "paid",
			"currency": "usd", "amount_total": 2500, "payment_intent": "pi_" + sessionID,
			"metadata": map[string]string{"workspace_id": c.form.Get("metadata[workspace_id]"), "lxc_amount": c.form.Get("metadata[lxc_amount]")}}
	}
	credited := func(ws string) (n int, amount int64, test bool) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(amount), 0), COALESCE(bool_and(test), false) FROM lxc_ledger
			WHERE workspace_id = $1 AND type = 'purchase'`, ws).Scan(&n, &amount, &test); err != nil {
			t.Fatal(err)
		}
		return
	}

	// Unset, a test workspace cannot pay, and is told which variables.
	if w := call(route(nil), "/v1/workspaces/"+testWS+"/billing/checkout", `{"usd_cents":2500}`); w.Code != http.StatusForbidden ||
		!strings.Contains(w.Body.String(), "LENS_STRIPE_TEST_SECRET_KEY and LENS_STRIPE_TEST_WEBHOOK_SECRET") {
		t.Fatalf("a test workspace's checkout with no test-mode Service = %d %s; want 403 naming both variables", w.Code, w.Body.String())
	}

	// 1. THE TEST USER TOPS UP $25: a Stripe TEST-MODE Checkout, then the payment with card 4242 credits it.
	if w := call(r, "/v1/workspaces/"+testWS+"/billing/checkout", `{"usd_cents":2500}`); w.Code != http.StatusOK {
		t.Fatalf("the test user's checkout = %d %s", w.Code, w.Body.String())
	}
	c := fake.last(t, "/v1/checkout/sessions")
	if c.key != "sk_test_b252" || c.form.Get("metadata[workspace_id]") != testWS {
		t.Fatalf("the test user's Checkout went to Stripe with key %q for %q; want the test-mode key sk_test_b252", c.key, c.form.Get("metadata[workspace_id]"))
	}
	deliver("/v1/billing/webhook/test", testSecret, "checkout.session.completed", paid(c, "cs_test_b252_topup"))
	if n, amount, test := credited(testWS); n != 1 || amount != 250_000_000 || !test {
		t.Fatalf("the test user's ledger has %d purchase credit(s) of %d µLXC, test=%v; want one of 250,000,000 (250 LXC), marked test", n, amount, test)
	}
	var status string
	var livemode, purchaseTest bool
	if err := pool.QueryRow(ctx, `SELECT status, livemode, test FROM lxc_purchases WHERE stripe_session_id = 'cs_test_b252_topup'`).
		Scan(&status, &livemode, &purchaseTest); err != nil || status != "completed" || livemode || !purchaseTest {
		t.Fatalf("the test user's purchase = %s livemode=%v test=%v (%v); want completed, test mode, marked test", status, livemode, purchaseTest, err)
	}
	if n := fake.count("/v1/payment_intents/pi_cs_test_b252_topup"); n != 0 {
		t.Errorf("the test card's fingerprint was fetched %d time(s) — every test user pays with 4242, so it would link them all", n)
	}

	// 2. THE TEST USER SUBSCRIBES TO PLUS: a test-mode plan, and the subscription Stripe sends grants the allowance.
	if w := call(r, "/v1/workspaces/"+testWS+"/billing/subscribe", `{"plan":"plus"}`); w.Code != http.StatusOK {
		t.Fatalf("the test user's subscribe = %d %s", w.Code, w.Body.String())
	}
	c = fake.last(t, "/v1/checkout/sessions")
	if c.key != "sk_test_b252" || c.form.Get("line_items[0][price]") != "price_test_plus" || c.form.Get("mode") != "subscription" {
		t.Fatalf("the test user's subscription Checkout = key %q price %q mode %q; want sk_test_b252, price_test_plus, subscription",
			c.key, c.form.Get("line_items[0][price]"), c.form.Get("mode"))
	}
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	deliver("/v1/billing/webhook/test", testSecret, "customer.subscription.created", map[string]any{
		"id": "sub_test_b252", "object": "subscription", "status": "active", "livemode": false,
		"customer": map[string]any{"id": "cus_test_b252"}, "cancel_at_period_end": false,
		"current_period_start": start.Unix(), "current_period_end": start.Add(30 * 24 * time.Hour).Unix(),
		"metadata": map[string]string{"workspace_id": testWS},
		"items": map[string]any{"object": "list", "data": []any{map[string]any{"id": "si_test_b252", "object": "subscription_item", "quantity": 1,
			"price": map[string]any{"id": "price_test_plus", "object": "price", "currency": "usd", "unit_amount": 2000}}}},
	})
	var granted int64
	var subTest, allowanceTest bool
	if err := pool.QueryRow(ctx, `SELECT a.granted_ulxc, a.test, s.test FROM subscription_allowance a
		JOIN subscriptions s USING (stripe_subscription_id) WHERE a.workspace_id = $1`, testWS).Scan(&granted, &allowanceTest, &subTest); err != nil {
		t.Fatalf("the test user's subscription and allowance: %v", err)
	}
	if granted <= 0 || !allowanceTest || !subTest {
		t.Fatalf("the test user's allowance = %d µLXC, allowance test=%v, subscription test=%v; want an allowance, both marked test", granted, allowanceTest, subTest)
	}

	// 3. A TEST-MODE PAYMENT FOR A REAL WORKSPACE CREDITS NOTHING, and a live one for the test workspace neither.
	deliver("/v1/billing/webhook/test", testSecret, "checkout.session.completed", paid(stripeCall{form: url.Values{
		"metadata[workspace_id]": {realWS}, "metadata[lxc_amount]": {"250000000"}}}, "cs_test_b252_across"))
	deliver("/v1/billing/webhook", liveSecret, "checkout.session.completed", paid(stripeCall{form: url.Values{
		"metadata[workspace_id]": {testWS}, "metadata[lxc_amount]": {"250000000"}}}, "cs_live_b252_across"))
	if n, _, _ := credited(realWS); n != 0 {
		t.Fatalf("a Stripe test-mode payment credited the real workspace %d time(s) — test money reached a real user", n)
	}
	if n, _, _ := credited(testWS); n != 1 {
		t.Fatalf("the test user has %d purchase credits after a live payment for it, want still 1", n)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM lxc_purchases WHERE stripe_session_id IN ('cs_test_b252_across', 'cs_live_b252_across')`).
		Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("%d purchase rows for payments of the other kind of workspace (%v), want 0", rows, err)
	}

	// 4. THE REAL USER'S CHECKOUT IS UNCHANGED: the live key, the live webhook, an unmarked credit.
	if w := call(r, "/v1/workspaces/"+realWS+"/billing/checkout", `{"usd_cents":2500}`); w.Code != http.StatusOK {
		t.Fatalf("the real user's checkout = %d %s", w.Code, w.Body.String())
	}
	c = fake.last(t, "/v1/checkout/sessions")
	if c.key != "sk_live_b252" || c.form.Get("metadata[workspace_id]") != realWS {
		t.Fatalf("the real user's Checkout went to Stripe with key %q; want the live key sk_live_b252", c.key)
	}
	deliver("/v1/billing/webhook", liveSecret, "checkout.session.completed", paid(c, "cs_live_b252_real"))
	if n, amount, test := credited(realWS); n != 1 || amount != 250_000_000 || test {
		t.Fatalf("the real user's ledger has %d purchase credit(s) of %d µLXC, test=%v; want one of 250,000,000, not test", n, amount, test)
	}
	if stripe.Key != "sk_live_b252" {
		t.Errorf("the process-wide Stripe key is %q after the test-mode Service was built; want the live key untouched", stripe.Key)
	}
}
