package billing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	stripe "github.com/stripe/stripe-go/v81"

	"github.com/talyvor/lens/internal/partners"
)

// B32.45 — Stripe Tax is the real tax partner. Its answer for each line is the Test partner's for the same sale: with
// B32.37's fixture rates and Talyvor's UK registration alone — what the Talyvor LTD sandbox holds — a GB consumer's
// $10.00 line taxes at 2000 bps, 2,000,000 µUSD; a DE business with a valid VAT number is reverse_charge; a DE
// consumer is not_registered.
//
// testdata/stripe_tax holds a response for each sale. Those whose ids read taxcalc_from_api_reference_ are written
// from Stripe's API reference, not recorded; with the sandbox's test key, TestStripeTax_LiveTestMode checks the same
// sales against Stripe itself, and with STRIPE_TAX_RECORD=1 records its answers over them:
//
//	STRIPE_TAX_TEST_KEY=sk_test_… STRIPE_TAX_RECORD=1 go test -run TestStripeTax_LiveTestMode ./internal/billing/

var stripeTaxSales = []struct {
	name     string
	customer partners.TaxParty
}{
	{"gb_consumer", partners.TaxParty{ID: "ws-gb", Country: "GB"}},
	{"de_business", partners.TaxParty{ID: "ws-de-business", Country: "DE", Business: true, TaxID: "DE123456789", TaxIDValid: true}},
	{"de_consumer", partners.TaxParty{ID: "ws-de-consumer", Country: "DE"}},
}

// fixtureTaxRows is the tax rows of testdata/tax/rates.csv, and Talyvor's UK registration.
type fixtureTaxRows []partners.TaxRateRow

func (f fixtureTaxRows) TaxRate(_ context.Context, jurisdiction, taxCode string, at time.Time) (partners.TaxRate, bool, error) {
	for _, r := range f {
		if r.Jurisdiction == jurisdiction && r.TaxCode == taxCode && !at.Before(r.ValidFrom) && (r.ValidTo == nil || at.Before(*r.ValidTo)) {
			return r.TaxRate, true, nil
		}
	}
	return partners.TaxRate{}, false, nil
}

func (fixtureTaxRows) TaxRegistration(_ context.Context, jurisdiction string, _ time.Time) (partners.TaxRegistration, bool, error) {
	return partners.TaxRegistration{Jurisdiction: "GB", Scheme: "GB VAT"}, jurisdiction == "GB", nil
}

// checkStripeTaxContract asks Stripe Tax, through api, and the Test partner over the fixture rows about each sale, and
// wants the same lines. before is called with each sale's name before Stripe is asked.
func checkStripeTaxContract(t *testing.T, api partners.StripeTaxAPI, at time.Time, before func(name string)) {
	t.Helper()
	f, err := os.Open("../partners/testdata/tax/rates.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := partners.ParseTaxRatesCSV(f)
	if err != nil {
		t.Fatal(err)
	}
	test := &partners.TestTaxPartner{Data: fixtureTaxRows(rows)}
	stripeTax := &partners.StripeTaxPartner{API: api, Rows: test}
	ctx := context.Background()
	for _, sale := range stripeTaxSales {
		req := partners.TaxRequest{Supplier: partners.TaxParty{ID: partners.SupplierTalyvor, Country: "GB"}, Customer: sale.customer,
			Lines: []partners.TaxLine{{Ref: "use-" + sale.name, TaxCode: "digital_service", AmountMicros: 10_000_000, Currency: "USD"}}, At: at}
		before(sale.name)
		got, err := stripeTax.Calculate(ctx, req)
		if err != nil {
			t.Fatalf("%s: %v", sale.name, err)
		}
		want, err := test.Calculate(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if got.Partner != "stripe" || !reflect.DeepEqual(got.Lines, want.Lines) || got.TaxMicros != want.TaxMicros {
			t.Errorf("%s: Stripe Tax answered %+v, the Test partner %+v", sale.name, got, want)
		}
	}
}

// The recorded responses, served to LiveStripe as Stripe would serve them, give the Test partner's lines; and what
// LiveStripe sends is the sale: the customer, a VAT number only for a business, the amount in cents, the
// electronically-supplied-services code, tax on top, each line's breakdown expanded, and the time of the sale.
func TestStripeTax_RecordedResponsesMatchTheTestPartner(t *testing.T) {
	var sale string
	sent := map[string]url.Values{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tax/calculations" || r.Header.Get("Authorization") != "Bearer sk_test_fixture" {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		_ = r.ParseForm()
		sent[sale] = r.PostForm
		body, err := os.ReadFile(filepath.Join("testdata", "stripe_tax", sale+".json"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	prev := stripe.GetBackend(stripe.APIBackend)
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{URL: stripe.String(srv.URL)}))
	t.Cleanup(func() { stripe.SetBackend(stripe.APIBackend, prev) })

	at := time.Date(2026, 10, 9, 15, 0, 0, 0, time.UTC)
	checkStripeTaxContract(t, NewTestModeStripe("sk_test_fixture", "", ""), at, func(name string) { sale = name })
	for name, want := range map[string]map[string]string{
		"gb_consumer": {"customer_details[address][country]": "GB", "customer_details[tax_ids][0][type]": ""},
		"de_business": {"customer_details[address][country]": "DE", "customer_details[tax_ids][0][type]": "eu_vat",
			"customer_details[tax_ids][0][value]": "DE123456789"},
		"de_consumer": {"customer_details[address][country]": "DE", "customer_details[tax_ids][0][type]": ""},
	} {
		for k, v := range map[string]string{"currency": "usd", "customer_details[address_source]": "billing",
			"line_items[0][amount]": "1000", "line_items[0][reference]": "0", "line_items[0][tax_code]": "txcd_10000000",
			"line_items[0][tax_behavior]": "exclusive", "expand[0]": "line_items", "expand[1]": "line_items.data.tax_breakdown",
			"tax_date": strconv.FormatInt(at.Unix(), 10)} {
			want[k] = v
		}
		for k, v := range want {
			if got := sent[name].Get(k); got != v {
				t.Errorf("%s: sent %s=%q, want %q", name, k, got, v)
			}
		}
	}
}

// recordingTaxAPI is LiveStripe, keeping the raw answer to each sale.
type recordingTaxAPI struct {
	*LiveStripe
	sale string
	raw  map[string][]byte
}

func (r *recordingTaxAPI) CalculateTax(ctx context.Context, params *stripe.TaxCalculationParams) (*stripe.TaxCalculation, error) {
	c, err := r.LiveStripe.CalculateTax(ctx, params)
	if err == nil && c.LastResponse != nil {
		r.raw[r.sale] = c.LastResponse.RawJSON
	}
	return c, err
}

// Stripe Tax itself, in the test mode of the sandbox whose key STRIPE_TAX_TEST_KEY is, gives the Test partner's lines
// for the same sales now. Skipped without the key.
func TestStripeTax_LiveTestMode(t *testing.T) {
	key := os.Getenv("STRIPE_TAX_TEST_KEY")
	if key == "" {
		t.Skip("STRIPE_TAX_TEST_KEY not set")
	}
	if LiveKey(key) {
		t.Fatal("STRIPE_TAX_TEST_KEY is a live key; this test calculates in test mode only")
	}
	api := &recordingTaxAPI{LiveStripe: NewTestModeStripe(key, "", ""), raw: map[string][]byte{}}
	checkStripeTaxContract(t, api, time.Now().UTC(), func(name string) { api.sale = name })
	if os.Getenv("STRIPE_TAX_RECORD") != "1" || t.Failed() {
		return
	}
	for name, raw := range api.raw {
		if err := os.WriteFile(filepath.Join("testdata", "stripe_tax", name+".json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
