package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
)

// B32.18 — a listing published with a per_use, a 30-day rent, a buy and a 30-day subscription of 100 uses reads back
// with all four; a second active commercial rent is refused and changes nothing; replacing the per_use price bills
// the next use at the new price while the past use's row keeps its own; a listing sold only by licence is not used.
func TestMarketOffers_PublishFourReadThemBackAndReplaceThePrice(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const seller, buyer = "ws-offer-seller", "ws-offer-buyer"
	stripeFake := &marketStripe{}
	store := market.NewStore(pool)
	bank := economy.NewDualTokenStore(nil, pool, nil)
	svc := billing.New(pool, bank, stripeFake, "whsec_offers").WithMarketBill(stripeFake, "price_market", "talyvor_marketplace_use", store)
	lens := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`))
	})
	r := chi.NewRouter()
	mountMarketRoutes(r, store)
	mountMarketUseRoutes(r, store, lens, svc, bank)
	call := func(ws, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	read := func(id string) market.Listing {
		t.Helper()
		code, body := call(buyer, http.MethodGet, "/v1/marketplace/listings/"+id, "")
		var l market.Listing
		if err := json.Unmarshal([]byte(body), &l); code != http.StatusOK || err != nil {
			t.Fatalf("read = %d %s", code, body)
		}
		return l
	}
	useRow := func(id string) (charge string, price int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT charge, price_ulxc FROM market_uses WHERE id = $1`, id).Scan(&charge, &price); err != nil {
			t.Fatal(err)
		}
		return charge, price
	}
	use := func(id string) (int, market.Use, string) {
		t.Helper()
		code, body := call(buyer, http.MethodPost, "/v1/workspaces/"+buyer+"/marketplace/listings/"+id+"/use", `{"variables":{"text":"Q3"}}`)
		var u market.Use
		_ = json.Unmarshal([]byte(body), &u)
		return code, u, body
	}

	const four = `[
		{"kind":"per_use","licence":"commercial","price_usd_micros":1000000},
		{"kind":"rent","licence":"commercial","price_usd_micros":20000000,"period_days":30},
		{"kind":"buy","licence":"commercial","price_usd_micros":150000000},
		{"kind":"subscribe","licence":"commercial","price_usd_micros":15000000,"period_days":30,"included_uses":100}]`
	code, body := call(seller, http.MethodPost, "/v1/workspaces/"+seller+"/marketplace/listings",
		`{"kind":"prompt","title":"Summariser","artifact":{"template":"Summarise {{text}}.","model":"gpt-5-mini"},"offers":`+four+`}`)
	if code != http.StatusCreated {
		t.Fatalf("publish = %d %s", code, body)
	}
	var published market.Listing
	_ = json.Unmarshal([]byte(body), &published)

	got := read(published.ID)
	type terms struct {
		kind, licence       string
		price               int64
		period, included    int
		hasPeriod, hasTerms bool
	}
	summary := func(o market.Offer) terms {
		t := terms{kind: o.Kind, licence: o.Licence, price: o.PriceUSDMicros, hasTerms: o.Terms == market.LicenceTerms[o.Licence]}
		if o.PeriodDays != nil {
			t.period, t.hasPeriod = *o.PeriodDays, true
		}
		if o.IncludedUses != nil {
			t.included = *o.IncludedUses
		}
		return t
	}
	want := []terms{
		{kind: "per_use", licence: "commercial", price: 1_000_000, hasTerms: true},
		{kind: "rent", licence: "commercial", price: 20_000_000, period: 30, hasPeriod: true, hasTerms: true},
		{kind: "subscribe", licence: "commercial", price: 15_000_000, period: 30, included: 100, hasPeriod: true, hasTerms: true},
		{kind: "buy", licence: "commercial", price: 150_000_000, hasTerms: true},
	}
	if len(got.Offers) != len(want) {
		t.Fatalf("the listing reads back %d offers %+v; want the four it was published with", len(got.Offers), got.Offers)
	}
	for i, o := range got.Offers {
		if summary(o) != want[i] || !strings.HasPrefix(o.ID, "ofr_") {
			t.Errorf("offer %d = %+v; want %+v", i, o, want[i])
		}
	}
	if got.PricePerUseULXC != 10_000_000 {
		t.Errorf("the listing's price per use = %d µLXC; want its per_use offer's 1,000,000 µUSD, 10,000,000 µLXC", got.PricePerUseULXC)
	}

	// A second active commercial rent is refused, and the four stay as they were.
	code, body = call(seller, http.MethodPut, "/v1/workspaces/"+seller+"/marketplace/listings/"+published.ID+"/offers",
		`{"offers":[{"kind":"rent","licence":"commercial","price_usd_micros":20000000,"period_days":30},
			{"kind":"rent","licence":"commercial","price_usd_micros":5000000,"period_days":7}]}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "one active commercial rent offer") {
		t.Fatalf("a second commercial rent = %d %s; want 400 naming the rule", code, body)
	}
	var active int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM market_offers WHERE listing_id = $1 AND active`, published.ID).Scan(&active); err != nil || active != 4 {
		t.Fatalf("%d active offers after the refusal (%v); want the four", active, err)
	}

	// The buyer uses it at the per_use price; the price is replaced; the next use bills the new price and the
	// first keeps its own. The offers whose terms did not change keep their ids.
	code, first, body := use(published.ID)
	if code != http.StatusOK {
		t.Fatalf("use = %d %s", code, body)
	}
	if charge, price := useRow(first.ID); charge != market.ChargeBilled || price != 10_000_000 {
		t.Fatalf("the first use's row = %s at %d µLXC; want billed at 10,000,000", charge, price)
	}
	replaced := strings.Replace(four, `"price_usd_micros":1000000}`, `"price_usd_micros":2000000}`, 1)
	code, body = call(seller, http.MethodPut, "/v1/workspaces/"+seller+"/marketplace/listings/"+published.ID+"/offers", `{"offers":`+replaced+`}`)
	if code != http.StatusOK {
		t.Fatalf("replace = %d %s", code, body)
	}
	after := read(published.ID)
	for i := 1; i < 4; i++ {
		if after.Offers[i].ID != got.Offers[i].ID {
			t.Errorf("the unchanged %s offer is %s now, was %s; want it kept", after.Offers[i].Kind, after.Offers[i].ID, got.Offers[i].ID)
		}
	}
	code, second, body := use(published.ID)
	if code != http.StatusOK {
		t.Fatalf("use after the replace = %d %s", code, body)
	}
	if charge, price := useRow(second.ID); charge != market.ChargeBilled || price != 20_000_000 {
		t.Errorf("the next use's row = %s at %d µLXC; want billed at the new 20,000,000", charge, price)
	}
	if _, price := useRow(first.ID); price != 10_000_000 {
		t.Errorf("the first use's row reads %d µLXC after the replace; want its own 10,000,000", price)
	}

	// Sold only by licence: no per_use commercial offer prices a use, so none is made (B32.19 adds the licences).
	code, body = call(seller, http.MethodPut, "/v1/workspaces/"+seller+"/marketplace/listings/"+published.ID+"/offers",
		`{"offers":[{"kind":"rent","licence":"commercial","price_usd_micros":20000000,"period_days":30}]}`)
	if code != http.StatusOK {
		t.Fatalf("rent only = %d %s", code, body)
	}
	var uses int
	if code, _, body := use(published.ID); code != http.StatusConflict {
		t.Errorf("a use of a listing sold only by rent = %d %s; want 409", code, body)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM market_uses WHERE listing_id = $1`, published.ID).Scan(&uses); err != nil || uses != 2 {
		t.Errorf("%d use rows (%v); want the two billed ones and nothing for the refused use", uses, err)
	}
}
