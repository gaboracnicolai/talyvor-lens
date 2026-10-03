package billing

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	stripe "github.com/stripe/stripe-go/v81"
)

// B17.23 — a seller's Stripe account is created with Accounts v2, as a recipient that can be sent
// transfers, and onboarded through a v2 account link. The fake answers only the v2 routes, at the version
// they are asked at; v1's POST /v1/accounts answers what Stripe now answers a new integration.
func TestB1723_ASellersAccountIsCreatedWithAccountsV2AndOnboardedByAV2Link(t *testing.T) {
	asked := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Stripe-Version") != connectAPIVersion || r.Header.Get("Content-Type") != "application/json" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"Stripe no longer recommends Accounts v1 for new Connect integrations."}}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		asked[r.URL.Path] = body
		switch r.URL.Path {
		case "/v2/core/accounts":
			_, _ = io.WriteString(w, `{"id":"acct_b1723","object":"v2.core.account","identity":{"country":"gb"}}`)
		case "/v2/core/account_links":
			_, _ = io.WriteString(w, `{"object":"v2.core.account_link","url":"https://connect.stripe.com/setup/e/acct_b1723/x"}`)
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

	l := &LiveStripe{key: "sk_test_b1723"}
	a, err := l.CreateConnectedAccount(context.Background(), "ws-b1723", "GB")
	if err != nil || a.ID != "acct_b1723" || a.Country != "GB" || a.PayoutsEnabled {
		t.Fatalf("CreateConnectedAccount = %+v, %v; want acct_b1723 in GB, not yet payable", a, err)
	}
	created, _ := json.Marshal(asked["/v2/core/accounts"])
	const want = `{"configuration":{"recipient":{"capabilities":{"stripe_balance":{"stripe_transfers":{"requested":true}}}}},` +
		`"dashboard":"express","defaults":{"responsibilities":{"fees_collector":"application","losses_collector":"application"}},` +
		`"identity":{"country":"gb"},"include":["identity"],"metadata":{"market_workspace_id":"ws-b1723"}}`
	if string(created) != want {
		t.Fatalf("the account Lens asked for:\n %s\nwant\n %s", created, want)
	}

	url, err := l.OnboardingLink(context.Background(), a.ID, "https://app.example/refresh", "https://app.example/return")
	if err != nil || url != "https://connect.stripe.com/setup/e/acct_b1723/x" {
		t.Fatalf("OnboardingLink = %q, %v; want Stripe's onboarding URL", url, err)
	}
	linked, _ := json.Marshal(asked["/v2/core/account_links"])
	const wantLink = `{"account":"acct_b1723","use_case":{"account_onboarding":{"configurations":["recipient"],` +
		`"refresh_url":"https://app.example/refresh","return_url":"https://app.example/return"},"type":"account_onboarding"}}`
	if string(linked) != wantLink {
		t.Fatalf("the onboarding link Lens asked for:\n %s\nwant\n %s", linked, wantLink)
	}
}
