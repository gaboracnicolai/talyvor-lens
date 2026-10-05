package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stripe/stripe-go/v81"

	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/market"
)

// B28.276 (B17.43) — Stripe's v2 API refuses a seller's account with a code and a message and no v1 type.
// Connect with Stripe answers 400 with Stripe's reason, not the 502 the seller read as "Nothing happened",
// and Lens records no account.
func TestB28276_ASellerAccountStripesV2APIRefusesAnswers400WithStripesReason(t *testing.T) {
	pool := agentRoutesDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-Id", "req_b28276")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"platform_registration_required",`+
			`"message":"The direct merchant has not signed up for Connect and cannot create connected accounts."}}`)
	}))
	t.Cleanup(srv.Close)
	prev := stripe.GetBackend(stripe.APIBackend)
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
		URL: stripe.String(srv.URL), MaxNetworkRetries: stripe.Int64(0),
	}))
	t.Cleanup(func() { stripe.SetBackend(stripe.APIBackend, prev) })

	r := chi.NewRouter()
	mountMarketPayoutRoutes(r, market.NewStore(pool), everyWorkspace(billing.NewTestModeStripe("sk_test_b28276", "", "")), nil, marketPayoutURLs{})
	code, why := b2612Post(t, r, "/v1/workspaces/ws-b2612/marketplace/payouts/connect", `{"country":"GB"}`)
	if code != http.StatusBadRequest || why != "Stripe says: The direct merchant has not signed up for Connect and cannot create connected accounts." {
		t.Fatalf("a seller account Stripe's v2 API refuses = %d %q, want 400 with Stripe's reason", code, why)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM market_sellers WHERE workspace_id = 'ws-b2612'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("market_sellers rows for the refused seller = %d, %v; want none", n, err)
	}
}
