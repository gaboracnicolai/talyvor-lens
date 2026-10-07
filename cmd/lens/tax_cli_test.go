package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/partners"
)

type taxFake struct {
	rows []partners.TaxRateRow
	regs []partners.TaxRegistration
}

func (f *taxFake) ImportRates(_ context.Context, rows []partners.TaxRateRow) (int, int, error) {
	f.rows = append(f.rows, rows...)
	return len(rows), 3, nil
}
func (f *taxFake) AddRegistration(_ context.Context, r partners.TaxRegistration) error {
	f.regs = append(f.regs, r)
	return nil
}
func (f *taxFake) Rates(context.Context) ([]partners.TaxRate, error) { return nil, nil }
func (f *taxFake) Registrations(context.Context) ([]partners.TaxRegistration, error) {
	return f.regs, nil
}

// B32.37 — `lens tax rates import <csv>` loads every line of a rates file, and `lens tax registrations add`
// records a registration under its scheme and number from its day; a registration missing one is refused.
func TestTaxCommand_ImportsRatesAndAddsRegistrations(t *testing.T) {
	ctx := context.Background()
	store := &taxFake{}
	var out bytes.Buffer
	if err := taxCommand(ctx, store, []string{"rates", "import", "../../internal/partners/testdata/tax/rates.csv"}, &out); err != nil {
		t.Fatal(err)
	}
	if len(store.rows) != 4 || store.rows[0].Jurisdiction != "GB" || store.rows[0].TaxCode != "digital_service" ||
		store.rows[2].Union != "EU" || store.rows[1].ValidTo == nil {
		t.Fatalf("imported %+v, want the fixture file's four rates", store.rows)
	}
	if !strings.Contains(out.String(), "imported 4 tax rates") {
		t.Fatalf("said %q", out.String())
	}

	if err := taxCommand(ctx, store, []string{"registrations", "add", "eu", "--scheme", "EU OSS non-Union", "--number", "EU826000001",
		"--from", "2026-11-01"}, &out); err != nil {
		t.Fatal(err)
	}
	want := partners.TaxRegistration{Jurisdiction: "EU", Scheme: "EU OSS non-Union", Number: "EU826000001",
		EffectiveFrom: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	if len(store.regs) != 1 || store.regs[0] != want {
		t.Fatalf("registered %+v, want %+v", store.regs, want)
	}
	if err := taxCommand(ctx, store, []string{"registrations", "add", "GB", "--scheme", "GB VAT"}, &out); err == nil {
		t.Fatal("a registration with no number or day was accepted")
	}
	out.Reset()
	if err := taxCommand(ctx, store, []string{"registrations"}, &out); err != nil || !strings.Contains(out.String(), "EU\tEU OSS non-Union\tEU826000001\tfrom 2026-11-01") {
		t.Fatalf("listed %q, %v", out.String(), err)
	}
}
