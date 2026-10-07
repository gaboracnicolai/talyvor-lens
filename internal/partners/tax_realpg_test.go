package partners

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

// B32.37 — the Test tax partner decides from the tax rows (0222), loaded from the fixtures in testdata/tax.

func taxPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv("LENS_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	name := fmt.Sprintf("lens_tax_%d", time.Now().UnixNano())
	ac, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ac.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		_ = ac.Close(ctx)
		t.Fatal(err)
	}
	_ = ac.Close(ctx)
	u, _ := url.Parse(admin)
	u.Path = "/" + name
	mc, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dbmigrate.Run(ctx, mc, migrations.FS); err != nil {
		_ = mc.Close(ctx)
		t.Fatal(err)
	}
	_ = mc.Close(ctx)
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		c, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			return
		}
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = c.Close(context.Background())
	})
	return pool
}

// loadTaxRegistrations adds every registration in testdata/tax/registrations.csv, as `lens tax registrations add`.
func loadTaxRegistrations(t *testing.T, store *TaxStore) {
	t.Helper()
	f, err := os.Open("testdata/tax/registrations.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cr := csv.NewReader(f)
	cr.Comment = '#'
	recs, err := cr.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs[1:] {
		from, err := time.Parse(time.DateOnly, rec[3])
		if err != nil {
			t.Fatal(err)
		}
		if err := store.AddRegistration(context.Background(), TaxRegistration{Jurisdiction: rec[0], Scheme: rec[1], Number: rec[2],
			EffectiveFrom: from}); err != nil {
			t.Fatal(err)
		}
	}
}

// With the fixture rows: a GB consumer's $10.00 line taxes at 2000 bps, 2,000,000 µUSD; a DE business with a valid
// VAT number gets reverse_charge and 0; a DE consumer gets not_registered without an EU OSS registration row and
// 1900 bps with one — and the rate in force when the sale is made, not today's. A tax code with no rate loaded is
// no_rate. The partner is the one the registry hands out.
func TestTestTaxPartner_DecidesFromTheTaxRows(t *testing.T) {
	ctx := context.Background()
	store := NewTaxStore(taxPool(t))
	f, err := os.Open("testdata/tax/rates.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := ParseTaxRatesCSV(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ImportRates(ctx, rows); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(nil)
	reg.UseTaxData(store)
	tax := reg.Tax()

	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	talyvor := TaxParty{ID: SupplierTalyvor, Country: "GB"}
	line := func(ref, code string) []TaxLine {
		return []TaxLine{{Ref: ref, TaxCode: code, AmountMicros: 10_000_000, Currency: "USD"}}
	}
	calc := func(customer TaxParty, at time.Time, lines []TaxLine) TaxLineResult {
		t.Helper()
		res, err := tax.Calculate(ctx, TaxRequest{Supplier: talyvor, Customer: customer, Lines: lines, At: at})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Lines) != 1 || res.TaxMicros != res.Lines[0].TaxMicros || res.Partner != "test" {
			t.Fatalf("result %+v: want one line from the test partner, its tax the total", res)
		}
		return res.Lines[0]
	}
	deConsumer := TaxParty{ID: "ws-de-consumer", Country: "DE"}

	if got := calc(deConsumer, now, line("use-de-unregistered", "digital_service")); got.Treatment != TaxNotRegistered ||
		got.TaxMicros != 0 || got.TaxableMicros != 0 {
		t.Fatalf("a DE consumer with no EU OSS registration loaded: %+v, want not_registered and no tax", got)
	}

	loadTaxRegistrations(t, NewTaxStore(store.pool))

	if got := calc(TaxParty{ID: "ws-gb", Country: "GB"}, now, line("use-gb", "digital_service")); got.Treatment != TaxStandard ||
		got.Jurisdiction != "GB" || got.RateBps != 2000 || got.TaxMicros != 2_000_000 || got.TaxableMicros != 10_000_000 ||
		got.Note != "Tax at 20% in GB" {
		t.Fatalf("a GB consumer's $10.00 line: %+v, want standard at 2000 bps, 2,000,000 µUSD", got)
	}

	vat, err := tax.ValidateTaxID(ctx, "DE", "DE 123 456 789")
	if err != nil || !vat.Valid || vat.Number != "DE123456789" {
		t.Fatalf("a well-formed DE VAT number: %+v, %v; want valid as DE123456789", vat, err)
	}
	deBusiness := TaxParty{ID: "ws-de-business", Country: "DE", Business: true, TaxID: vat.Number, TaxIDValid: vat.Valid}
	if got := calc(deBusiness, now, line("use-de-business", "digital_service")); got.Treatment != TaxReverseCharge ||
		got.TaxMicros != 0 || got.RateBps != 0 || got.Note == "" {
		t.Fatalf("a DE business with a valid VAT number: %+v, want reverse_charge, 0 and its note", got)
	}

	if got := calc(deConsumer, now, line("use-de-oss", "digital_service")); got.Treatment != TaxStandard ||
		got.RateBps != 1900 || got.TaxMicros != 1_900_000 {
		t.Fatalf("a DE consumer with the EU OSS registration loaded: %+v, want standard at 1900 bps", got)
	}
	if got := calc(deConsumer, time.Date(2020, 8, 1, 0, 0, 0, 0, time.UTC), line("use-de-2020", "digital_service")); got.RateBps != 1600 {
		t.Fatalf("a DE consumer in August 2020: %+v, want that year's 1600 bps", got)
	}
	if got := calc(TaxParty{ID: "ws-gb", Country: "GB"}, now, line("use-gb-ebook", "ebook")); got.Treatment != TaxNoRate || got.TaxMicros != 0 {
		t.Fatalf("a tax code with no rate loaded: %+v, want no_rate and no tax", got)
	}
}
