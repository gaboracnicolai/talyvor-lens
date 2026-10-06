package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stripe/stripe-go/v81"

	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/market"
)

// B35.4 — Connect with Stripe gives Stripe the signed-in owner's email as the account's contact_email, which
// Stripe requires of a recipient account; a test workspace gives its own test address; and an owner with no
// email is told to add one, with no call to Stripe and no account recorded. The stub refuses an account
// without a contact email as Stripe does.
func TestB354_ConnectWithStripeSendsTheSellersEmail(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ('s-b354', 's-b354', 's-b354', true)`); err != nil {
		t.Fatal(err)
	}
	var calls []string
	created := map[string]map[string]any{} // by workspace
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		calls = append(calls, r.URL.Path)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/v2/core/accounts":
			if email, _ := body["contact_email"].(string); email == "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"code":"parameter_invalid","message":"Some fields in the request were invalid: `+
					`'configuration.recipient: If configuration.recipient is supplied, the Account must have a contact email.'"}}`)
				return
			}
			ws := body["metadata"].(map[string]any)["market_workspace_id"].(string)
			created[ws] = body
			_, _ = io.WriteString(w, `{"id":"acct_`+ws+`","object":"v2.core.account","identity":{"country":"gb"}}`)
		case "/v2/core/account_links":
			_, _ = io.WriteString(w, `{"object":"v2.core.account_link","url":"https://connect.stripe.com/setup/e/x"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	prev := stripe.GetBackend(stripe.APIBackend)
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
		URL: stripe.String(srv.URL), MaxNetworkRetries: stripe.Int64(0),
	}))
	t.Cleanup(func() { stripe.SetBackend(stripe.APIBackend, prev) })
	r := chi.NewRouter()
	mountMarketPayoutRoutes(r, market.NewStore(pool), everyWorkspace(billing.NewTestModeStripe("sk_test_b354", "", "")), nil, marketPayoutURLs{})

	// The signed-in owner's email is the account's contact email.
	if code, why := b2612Post(t, r, "/v1/workspaces/u-b354-owner/marketplace/payouts/connect", `{"country":"GB","email":"owner@seller.example"}`); code != http.StatusOK {
		t.Fatalf("an owner with an email connects = %d %q, want 200 and Stripe's onboarding", code, why)
	}
	if got := created["u-b354-owner"]["contact_email"]; got != "owner@seller.example" {
		t.Fatalf("contact_email Stripe was sent for the owner = %v, want owner@seller.example", got)
	}

	// A test workspace's owner has no email: it sends its own test address.
	if code, why := b2612Post(t, r, "/v1/workspaces/s-b354/marketplace/payouts/connect", `{"country":"GB"}`); code != http.StatusOK {
		t.Fatalf("a test workspace connects = %d %q, want 200 and Stripe's onboarding", code, why)
	}
	if got := created["s-b354"]["contact_email"]; got != "synthetic+s-b354@example.com" {
		t.Fatalf("contact_email Stripe was sent for the test workspace = %v, want synthetic+s-b354@example.com", got)
	}

	// An owner with no email is told to add one, and Stripe is not asked.
	before := len(calls)
	code, why := b2612Post(t, r, "/v1/workspaces/u-b354-noemail/marketplace/payouts/connect", `{"country":"GB"}`)
	if code != http.StatusBadRequest || !strings.Contains(why, "Add an email address to your sign-in to connect with Stripe") {
		t.Fatalf("an owner with no email = %d %q, want 400 asking them to add one", code, why)
	}
	if len(calls) != before {
		t.Fatalf("Stripe was asked %v for an owner with no email; want no call", calls[before:])
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM market_sellers WHERE workspace_id = 'u-b354-noemail'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("market_sellers rows for the owner with no email = %d, %v; want none", n, err)
	}
}
