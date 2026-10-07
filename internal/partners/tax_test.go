package partners

import (
	"context"
	"testing"
)

// B32.37 — ValidateTaxID checks a number's format for its country and the fixture list, and asks no authority.
func TestTestTaxPartner_ValidateTaxID(t *testing.T) {
	ctx := context.Background()
	p := &TestTaxPartner{}
	for _, c := range []struct {
		country, id, number string
		valid               bool
	}{
		{"DE", "DE123456789", "DE123456789", true},
		{"de", "123456789", "DE123456789", true},
		{"GB", "GB 123 4567 89", "GB123456789", true},
		{"GR", "123456789", "EL123456789", true},
		{"DE", "DE12345", "DE12345", false},           // malformed: too short
		{"FR", "FR1234567890", "FR1234567890", false}, // malformed: one digit short
		{"GB", "GB999999999", "GB999999999", false},   // well-formed, on the fixture list
		{"US", "12-3456789", "US123456789", false},    // a country test mode does not check
	} {
		got, err := p.ValidateTaxID(ctx, c.country, c.id)
		if err != nil {
			t.Fatalf("%s %s: %v", c.country, c.id, err)
		}
		if got.Valid != c.valid || got.Number != c.number || (!got.Valid && got.Detail == "") {
			t.Errorf("%s %q: %+v, want valid=%v as %s, and why when not", c.country, c.id, got, c.valid, c.number)
		}
	}
	if _, err := p.ValidateTaxID(ctx, "DE", " "); err == nil {
		t.Error("an empty number was checked")
	}
}

// Tax is rounded half-up per line: 0.5 µUSD rounds up, 0.4 down.
func TestTaxHalfUp(t *testing.T) {
	for _, c := range []struct {
		amount int64
		bps    int
		want   int64
	}{{5, 1000, 1}, {4, 1000, 0}, {10_000_000, 2000, 2_000_000}, {9_999_999, 1950, 1_950_000}} {
		if got := taxHalfUp(c.amount, c.bps); got != c.want {
			t.Errorf("%d at %d bps: %d, want %d", c.amount, c.bps, got, c.want)
		}
	}
}

// The registry hands out the Test tax partner until a real one is configured.
func TestRegistry_TaxIsTestUntilConfigured(t *testing.T) {
	r := NewRegistry(nil)
	if got := r.Tax().Name(); got != "test" {
		t.Fatalf("the registry handed out %q, want test", got)
	}
	if err := r.Configure(ServiceTax, realTax{&TestTaxPartner{}}); err != nil {
		t.Fatal(err)
	}
	if got := r.Tax().Name(); got != "real" {
		t.Fatalf("with a real tax partner configured the registry handed out %q", got)
	}
}

type realTax struct{ *TestTaxPartner }

func (realTax) Name() string { return "real" }
