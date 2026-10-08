package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/ecbrate"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/taxprofile"
)

// B32.51 — at a fixture rate (1 EUR = 1.10 USD = 0.85 GBP), a $20.00 rent offer reads for a GB consumer as £18.55: the
// price with the tax partner's 20% VAT, $24.00, converted and rounded to the penny, "incl. VAT"; for a GB business as
// £15.45 "+ VAT"; asked in euros, the consumer's catalog shows €21.82; its US-dollar price stays 20,000,000 µUSD, and
// the read says the charge is in US dollars on the monthly bill. A currency that is not an ISO code is refused.
func TestMarketDisplay_AGBConsumerSeesVATIncludedInPoundsAndABusinessPlusVAT(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const seller, consumer, business = "ws-display-seller", "ws-display-gb-consumer", "ws-display-gb-business"
	for _, ws := range []string{seller, consumer, business} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1) ON CONFLICT (id) DO NOTHING`, ws); err != nil {
			t.Fatal(err)
		}
	}
	taxData := partners.NewTaxStore(pool)
	f, err := os.Open("../../internal/partners/testdata/tax/rates.csv")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := partners.ParseTaxRatesCSV(f)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := taxData.ImportRates(ctx, rows); err != nil {
		t.Fatal(err)
	}
	if err := taxData.AddRegistration(ctx, partners.TaxRegistration{Jurisdiction: "GB", Scheme: "GB VAT", Number: "GB000000001",
		EffectiveFrom: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ecb_reference_rates (rate_date, currency, per_eur) VALUES
		(CURRENT_DATE - 1, 'USD', 1.1000), (CURRENT_DATE - 1, 'GBP', 0.8500) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	registry := partners.NewRegistry(economy.NewDualTokenStore(nil, pool, nil))
	registry.UseTaxData(taxData)
	profiles := taxprofile.NewStore(pool, registry)
	store := market.NewStore(pool)
	store.SetTax(market.Tax{Partners: registry, Buyers: profiles, Registrations: taxData})
	store.SetDisplayRates(ecbrate.New(pool, ""))
	if _, err := profiles.Put(ctx, consumer, taxprofile.Input{LegalName: "Ana Smith", Country: "GB"}); err != nil {
		t.Fatal(err)
	}
	if p, err := profiles.Put(ctx, business, taxprofile.Input{LegalName: "Widgets Ltd", Country: "GB", TaxID: "GB123456789"}); err != nil || !p.TaxIDValid {
		t.Fatalf("the business's profile = %+v, %v; want its VAT number valid", p, err)
	}

	r := chi.NewRouter()
	mountMarketRoutes(r, store)
	call := func(ws, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	code, body := call(seller, http.MethodPost, "/v1/workspaces/"+seller+"/marketplace/listings",
		`{"kind":"prompt","title":"Summariser","visibility":"public","artifact":{"template":"Summarise {{text}}.","model":"gpt-5-mini"},
		  "offers":[{"kind":"rent","licence":"commercial","price_usd_micros":20000000,"period_days":30}]}`)
	var published market.Listing
	if err := json.Unmarshal([]byte(body), &published); code != http.StatusCreated || err != nil {
		t.Fatalf("publish = %d %s", code, body)
	}
	offerAs := func(ws, path string) (market.Offer, string) {
		t.Helper()
		code, body := call(ws, http.MethodGet, path, "")
		if code != http.StatusOK {
			t.Fatalf("%s reads %s = %d %s", ws, path, code, body)
		}
		var l market.Listing
		if strings.Contains(path, "?") && !strings.Contains(path, published.ID) {
			var page struct{ Listings []market.Listing }
			_ = json.Unmarshal([]byte(body), &page)
			for _, x := range page.Listings {
				if x.ID == published.ID {
					l = x
				}
			}
		} else {
			_ = json.Unmarshal([]byte(body), &l)
		}
		if len(l.Offers) != 1 || l.Offers[0].Display == nil {
			t.Fatalf("%s reads %s: %s; want its one offer with a display price", ws, path, body)
		}
		return l.Offers[0], l.PriceNote
	}
	check := func(who string, o market.Offer, note string, want market.Display) {
		t.Helper()
		got := *o.Display
		if o.PriceUSDMicros != 20_000_000 || got.Currency != want.Currency || got.AmountMinor != want.AmountMinor ||
			got.IncludesTax != want.IncludesTax || got.TaxLabel != want.TaxLabel || got.Rate != want.Rate || got.Source != "ecb" ||
			got.RateDate == nil || !strings.HasPrefix(note, "Charged in US dollars on your monthly marketplace bill") {
			t.Fatalf("%s sees $%d µUSD as %+v, note %q; want the US-dollar price 20,000,000 µUSD beside %+v, charged in US dollars",
				who, o.PriceUSDMicros, got, note, want)
		}
	}

	o, note := offerAs(consumer, "/v1/marketplace/listings/"+published.ID)
	check("the GB consumer", o, note, market.Display{Currency: "GBP", AmountMinor: 18_55, IncludesTax: true, TaxLabel: "incl. VAT", Rate: "0.772727"})
	o, note = offerAs(business, "/v1/marketplace/listings/"+published.ID)
	check("the GB business", o, note, market.Display{Currency: "GBP", AmountMinor: 15_45, TaxLabel: "+ VAT", Rate: "0.772727"})
	o, note = offerAs(consumer, "/v1/marketplace/listings?currency=eur")
	check("the GB consumer in euros", o, note, market.Display{Currency: "EUR", AmountMinor: 21_82, IncludesTax: true, TaxLabel: "incl. VAT", Rate: "0.909091"})

	if code, body := call(consumer, http.MethodGet, "/v1/marketplace/listings/"+published.ID+"?currency=pounds", ""); code != http.StatusBadRequest {
		t.Fatalf("?currency=pounds = %d %s; want 400", code, body)
	}
}
