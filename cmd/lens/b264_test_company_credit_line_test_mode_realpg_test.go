package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	stripe "github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/webhook"

	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/economy"
)

// B26.4 — AFTER THE LIVE SWITCH A TEST COMPANY'S CREDIT LINE IS INVOICED IN STRIPE TEST MODE, AND A REAL
// COMPANY'S WITH THE MAIN KEY.
//
// Wired as main.go wires it (creditLineInvoicers) once LENS_STRIPE_SECRET_KEY is live, against B25.6's Stripe
// recorder of the key each call carried. Each company's agent draws on its line this month; next month's run
// invoices it. Every assertion that an invoice was made is on its credit_line_invoices row.
func TestB264_ATestCompanysCreditLineIsInvoicedInStripeTestModeAfterTheLiveSwitch(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const (
		tCo, rCo         = "s-b264-co", "u-b264-co"
		liveKey, testKey = "sk_live_b264", "sk_test_b264"
		testSecret       = "whsec_b264_test"
		lxc              = int64(1_000_000)
	)
	bank := economy.NewDualTokenStore(nil, pool, nil)
	for _, ws := range []string{tCo, rCo} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, $2)`,
			ws, strings.HasPrefix(ws, "s-")); err != nil {
			t.Fatal(err)
		}
		if err := bank.SetCompany(ctx, ws, true); err != nil {
			t.Fatal(err)
		}
		if _, err := bank.SetCreditLine(ctx, ws, 100*lxc, "test"); err != nil {
			t.Fatal(err)
		}
		agent, err := bank.CreateAgent(ctx, ws, "buyer", "owner-"+ws)
		if err != nil {
			t.Fatal(err)
		}
		if err := bank.AttachAgentKey(ctx, ws, agent.ID, "key-"+ws); err != nil {
			t.Fatal(err)
		}
		if err := bank.SpendLXCForAgent(ctx, "key-"+ws, ws, uuid.NewString(), 30*lxc, "a model call", economy.AgentDebitMeta{}); err != nil {
			t.Fatalf("%s spending 30 LXC on its line: %v", ws, err)
		}
	}

	fake := &b256Stripe{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	prevBackend, prevKey := stripe.GetBackend(stripe.APIBackend), stripe.Key
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{URL: stripe.String(srv.URL)}))
	t.Cleanup(func() { stripe.SetBackend(stripe.APIBackend, prevBackend); stripe.Key = prevKey })
	liveStripe, testStripe := billing.NewLiveStripe(liveKey, "", ""), billing.NewTestModeStripe(testKey, "", "")
	nextMonth := time.Now().UTC().AddDate(0, 1, 0)
	invoice := func(bills []creditLineInvoicer) (calls []b256Call, errs []error) {
		calls = fake.during(func() {
			for _, b := range bills {
				if _, err := b.InvoiceCreditLines(ctx, nextMonth); err != nil {
					errs = append(errs, err)
				}
			}
		})
		return calls, errs
	}
	invoiced := func(ws string) (stripeID string) {
		_ = pool.QueryRow(ctx, `SELECT stripe_invoice_id FROM credit_line_invoices WHERE workspace_id = $1 AND amount_cents = 300`, ws).Scan(&stripeID)
		return stripeID
	}
	// keys is which key each invoice call for ws carried.
	keys := func(calls []b256Call, ws string) map[string]bool {
		got := map[string]bool{}
		for _, c := range calls {
			if strings.HasPrefix(c.path, "/v1/invoice") &&
				(c.form.Get("metadata[credit_line_workspace_id]") == ws || strings.Contains(c.path, "in_"+ws) || c.form.Get("invoice") == "in_"+ws) {
				got[c.key] = true
			}
		}
		return got
	}

	// 1. UNSET: a live main key and no test-mode key. The real company is invoiced with the main key; the test
	// company's draws are refused, naming the variables, and nothing about it reaches Stripe.
	calls, errs := invoice(creditLineInvoicers(billing.New(pool, bank, liveStripe, "whsec_b264_live"), liveStripe, true, nil, nil))
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), tCo) || !strings.Contains(errs[0].Error(), "LENS_STRIPE_TEST_SECRET_KEY") {
		t.Fatalf("invoicing with no test-mode key = %v; want the test company refused, naming LENS_STRIPE_TEST_SECRET_KEY", errs)
	}
	if id := invoiced(tCo); id != "" || len(keys(calls, tCo)) != 0 {
		t.Fatalf("the test company was invoiced (%q) with no test-mode key; calls %+v", id, calls)
	}
	if got := keys(calls, rCo); invoiced(rCo) != "in_"+rCo || len(got) != 1 || !got[liveKey] {
		t.Fatalf("the real company's invoice: row %q, keys %v; want in_%s made with only %s", invoiced(rCo), got, rCo, liveKey)
	}

	// 2. SET: the test workspaces have their own Service on the test-mode key. The test company's draws go on a
	// test-mode invoice and the live Service, which keeps only real companies', asks Stripe for nothing.
	live := billing.New(pool, bank, liveStripe, "whsec_b264_live").ForTestWorkspaces(false)
	test := billing.New(pool, bank, testStripe, testSecret).ForTestWorkspaces(true)
	calls, errs = invoice(creditLineInvoicers(live, liveStripe, true, test, testStripe))
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if got := keys(calls, tCo); invoiced(tCo) != "in_"+tCo || len(got) != 1 || !got[testKey] {
		t.Fatalf("the test company's invoice: row %q, keys %v; want in_%s made with only %s", invoiced(tCo), got, tCo, testKey)
	}
	for _, c := range calls {
		if c.key != testKey {
			t.Errorf("the run with a test-mode key sent %s %s with %q; want only the test company's, with %s", c.method, c.path, c.key, testKey)
		}
	}

	// 3. PAID: the test-mode webhook records its payment, so the line lends again.
	payload, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("evt_b264_%d", time.Now().UnixNano()), "object": "event",
		"type": "invoice.paid", "created": time.Now().Unix(), "livemode": false, "data": map[string]any{"object": map[string]any{"id": "in_" + tCo}}})
	req := httptest.NewRequest(http.MethodPost, "/v1/billing/webhook/test", strings.NewReader(string(payload)))
	req.Header.Set("Stripe-Signature", webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: payload, Secret: testSecret}).Header)
	w := httptest.NewRecorder()
	if test.HandleWebhook(w, req); w.Code != http.StatusOK {
		t.Fatalf("invoice.paid on the test-mode webhook = %d %s", w.Code, w.Body.String())
	}
	var paid bool
	if err := pool.QueryRow(ctx, `SELECT paid_at IS NOT NULL FROM credit_line_invoices WHERE stripe_invoice_id = $1`, "in_"+tCo).Scan(&paid); err != nil || !paid {
		t.Fatalf("the test company's paid invoice: paid=%v (%v); want paid_at recorded", paid, err)
	}
}
