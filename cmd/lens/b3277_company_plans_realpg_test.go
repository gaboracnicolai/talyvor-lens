package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	stripe "github.com/stripe/stripe-go/v81"

	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/economy"
)

// B32.77 — GET /v1/billing/plans gives /pricing Team's and Business's prices, the BYOK add-on's and what
// Enterprise costs from, each Price read from Stripe, beside Plus, Pro and Max unchanged; a Price the Service
// does not sell is left out, never zero.
func TestB3277_ThePlansReadPricesTeamBusinessBYOKAndEnterprise(t *testing.T) {
	pool := agentRoutesDB(t)
	const key = "sk_test_b3277"
	srv := httptest.NewServer(b28439Stripe{})
	t.Cleanup(srv.Close)
	prevBackend, prevKey := stripe.GetBackend(stripe.APIBackend), stripe.Key
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{URL: stripe.String(srv.URL)}))
	t.Cleanup(func() { stripe.SetBackend(stripe.APIBackend, prevBackend); stripe.Key = prevKey })

	enterpriseFrom, err := billing.EnterpriseFromUSDCents("")
	if err != nil || enterpriseFrom != 250_000 {
		t.Fatalf("LENS_ENTERPRISE_FROM_USD_CENTS unset = %d, %v; want Nicolai's 250000", enterpriseFrom, err)
	}
	if _, err := billing.EnterpriseFromUSDCents("2500.00"); err == nil {
		t.Error("LENS_ENTERPRISE_FROM_USD_CENTS=2500.00 was taken; want it refused — whole cents only")
	}

	read := func(sold map[string]string) (int, map[string]json.RawMessage) {
		t.Helper()
		liveStripe := billing.NewLiveStripe(key, "", "")
		svc := billing.New(pool, economy.NewDualTokenStore(nil, pool, nil), liveStripe, "whsec_b3277").WithPlans(liveStripe, sold)
		r := chi.NewRouter()
		r.Get("/v1/billing/plans", newPlansHandler(newBillingRouter(svc, nil, func(string) bool { return false }, key, "", ""), enterpriseFrom))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/billing/plans", nil))
		var body map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("GET /v1/billing/plans = %d %s", w.Code, w.Body.String())
		}
		return w.Code, body
	}

	// With Stripe test Prices for every plan, the read prices them all.
	code, body := read(map[string]string{"plus": "price_plus", "pro": "price_pro", "max": "price_max",
		"team": "price_team", "business": "price_business", "byok": "price_byok"})
	if code != http.StatusOK {
		t.Fatalf("GET /v1/billing/plans = %d", code)
	}
	for field, want := range map[string]string{
		"plans": `[{"id":"plus","usd_cents":2000,"included_ulxc":170820000},{"id":"pro","usd_cents":10000,"included_ulxc":864900000},` +
			`{"id":"max","usd_cents":20000,"included_ulxc":1732500000}]`,
		"company_plans":             `[{"id":"team","usd_cents":4900},{"id":"business","usd_cents":29900}]`,
		"byok_add_on_usd_cents":     `19900`,
		"enterprise_from_usd_cents": `250000`,
	} {
		if string(body[field]) != want {
			t.Errorf("%s = %s; want %s", field, body[field], want)
		}
	}

	// A Service that sells only Plus leaves Team, Business and BYOK out rather than pricing them at zero.
	code, body = read(map[string]string{"plus": "price_plus"})
	if code != http.StatusOK || string(body["company_plans"]) != `[]` {
		t.Errorf("with only Plus sold: %d, company_plans = %s; want 200 and []", code, body["company_plans"])
	}
	if v, ok := body["byok_add_on_usd_cents"]; ok {
		t.Errorf("with BYOK not sold, byok_add_on_usd_cents = %s; want it absent", v)
	}
	if string(body["enterprise_from_usd_cents"]) != `250000` {
		t.Errorf("enterprise_from_usd_cents = %s; want 250000 whatever Stripe sells", body["enterprise_from_usd_cents"])
	}
}
