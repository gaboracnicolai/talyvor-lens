package billing

import (
	"context"
	"net/http"
	"strconv"
	"testing"
)

// B22.2 — a top-up paid in EUR and one paid in GBP, as Stripe's Adaptive Pricing reports them: the session's
// currency and amount_total are what the customer paid in their own currency, and currency_conversion holds
// the USD source amount Stripe converted from. Each lands as the USD credits it bought, on the ledger, with
// what was actually paid beside it; Lens converts nothing.
func TestWebhook_AdaptivePricing_EURAndGBPLandAsTheirUSDCredits(t *testing.T) {
	svc, pool, _ := newBillingService(t)
	ctx := context.Background()
	for _, c := range []struct {
		ws, sess, currency string
		paid, usdCents     int64
		fxRate             string
	}{
		{"ws_topup_eur", "cs_topup_eur", "eur", 918, 1000, "0.918"},   // $10.00 shown as €9.18
		{"ws_topup_gbp", "cs_topup_gbp", "gbp", 1861, 2500, "0.7444"}, // $25.00 shown as £18.61
	} {
		seedWS(t, pool, c.ws)
		obj := sessionObj(c.sess, c.ws, c.paid, c.currency, "paid", "pi_"+c.sess, micro(float64(c.usdCents)/10))
		obj["currency_conversion"] = map[string]any{"amount_subtotal": c.usdCents, "amount_total": c.usdCents,
			"fx_rate": c.fxRate, "source_currency": "usd"}
		body, sig := signed(testWebhookSecret, "evt_"+c.sess, "checkout.session.completed", obj)
		if got := post(svc, body, sig); got != http.StatusOK {
			t.Fatalf("%s: webhook = %d, want 200", c.currency, got)
		}
		assertStatus(t, pool, c.sess, "completed")
		var amount int64
		var usdCents, paidCurrency, paidAmount string
		if err := pool.QueryRow(ctx, `SELECT amount, metadata->>'usd_cents', metadata->>'paid_currency', metadata->>'paid_amount'
			FROM lxc_ledger WHERE workspace_id = $1 AND type = 'purchase'`, c.ws).Scan(&amount, &usdCents, &paidCurrency, &paidAmount); err != nil {
			t.Fatalf("%s: the purchase's ledger row: %v", c.currency, err)
		}
		want := micro(float64(c.usdCents) / 10) // $0.10 per LXC
		if amount != want || balance(t, pool, c.ws) != want {
			t.Errorf("%s: credited %d µLXC (balance %d), want %d for $%d.%02d", c.currency, amount, balance(t, pool, c.ws), want, c.usdCents/100, c.usdCents%100)
		}
		if paidCurrency != c.currency || paidAmount != strconv.FormatInt(c.paid, 10) || usdCents != strconv.FormatInt(c.usdCents, 10) {
			t.Errorf("%s: the row says paid %s %s for %s USD cents, want %s %d for %d", c.currency, paidAmount, paidCurrency, usdCents, c.currency, c.paid, c.usdCents)
		}
	}
}
