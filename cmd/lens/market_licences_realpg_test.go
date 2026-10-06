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

// B32.19 — over HTTP: renting a listing takes an Idempotency-Key (none is refused), writes one billed rent row on the
// buyer's bill and answers the licence (201); the same key again answers the same licence (200) and buys nothing; the
// next use is 'licensed' under it, and the buyer's licences list it with that use counted.
func TestMarketLicences_RentOverHTTPCoversTheNextUse(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const seller, buyer = "ws-lic-seller", "ws-lic-buyer"
	stripeFake := &marketStripe{}
	store := market.NewStore(pool)
	bank := economy.NewDualTokenStore(nil, pool, nil)
	svc := billing.New(pool, bank, stripeFake, "whsec_licences").WithMarketBill(stripeFake, "price_market", "talyvor_marketplace_use", store)
	lens := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`))
	})
	r := chi.NewRouter()
	mountMarketRoutes(r, store)
	mountMarketUseRoutes(r, store, lens, svc, bank)
	call := func(ws, method, path, key, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}

	code, body := call(seller, http.MethodPost, "/v1/workspaces/"+seller+"/marketplace/listings", "",
		`{"kind":"prompt","title":"Summariser","artifact":{"template":"Summarise {{text}}.","model":"gpt-5-mini"},"offers":[
			{"kind":"per_use","licence":"commercial","price_usd_micros":1000000},
			{"kind":"rent","licence":"commercial","price_usd_micros":20000000,"period_days":30}]}`)
	if code != http.StatusCreated {
		t.Fatalf("publish = %d %s", code, body)
	}
	var l market.Listing
	_ = json.Unmarshal([]byte(body), &l)
	rentOffer := ""
	for _, o := range l.Offers {
		if o.Kind == market.OfferRent {
			rentOffer = o.ID
		}
	}
	licences := "/v1/workspaces/" + buyer + "/marketplace/listings/" + l.ID + "/licences"
	rent := `{"offer_id":"` + rentOffer + `"}`

	if code, body := call(buyer, http.MethodPost, licences, "", rent); code != http.StatusBadRequest {
		t.Fatalf("a rent with no Idempotency-Key = %d %s; want 400", code, body)
	}
	code, body = call(buyer, http.MethodPost, licences, "rent-b3219", rent)
	var lic market.Licence
	if err := json.Unmarshal([]byte(body), &lic); code != http.StatusCreated || err != nil || lic.Status != market.LicenceActive || lic.Charge != market.ChargeBilled {
		t.Fatalf("rent = %d %s; want 201 with an active, billed licence", code, body)
	}
	var charge, kind string
	var price int64
	if err := pool.QueryRow(ctx, `SELECT charge, use_kind, price_ulxc FROM market_uses WHERE licence_id = $1 AND use_kind = 'rent'`, lic.ID).
		Scan(&charge, &kind, &price); err != nil || charge != market.ChargeBilled || price != 200_000_000 {
		t.Fatalf("the rent's row = %s %s at %d µLXC, %v; want one billed rent at 200,000,000 µLXC", charge, kind, price, err)
	}
	code, body = call(buyer, http.MethodPost, licences, "rent-b3219", rent)
	var again market.Licence
	if err := json.Unmarshal([]byte(body), &again); code != http.StatusOK || err != nil || again.ID != lic.ID {
		t.Fatalf("the same key again = %d %s; want 200 with licence %s", code, body, lic.ID)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM market_uses WHERE buyer_workspace_id = $1`, buyer).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("the buyer's rows after a replay = %d, %v; want the one rent", rows, err)
	}

	code, body = call(buyer, http.MethodPost, "/v1/workspaces/"+buyer+"/marketplace/listings/"+l.ID+"/use", "", `{"variables":{"text":"Q3"}}`)
	var u market.Use
	if err := json.Unmarshal([]byte(body), &u); code != http.StatusOK || err != nil || u.Charge != market.ChargeLicensed || u.LicenceID != lic.ID {
		t.Fatalf("a use under the rent = %d %s; want licensed under %s", code, body, lic.ID)
	}
	code, body = call(buyer, http.MethodGet, "/v1/workspaces/"+buyer+"/marketplace/licences", "", "")
	var list struct {
		Licences []market.Licence `json:"licences"`
	}
	if err := json.Unmarshal([]byte(body), &list); code != http.StatusOK || err != nil || len(list.Licences) != 1 || list.Licences[0].UsesCovered != 1 {
		t.Fatalf("the buyer's licences = %d %s; want the rent, having covered 1 use", code, body)
	}
}
