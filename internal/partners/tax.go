package partners

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
)

// TaxPartner works out the tax on a sale and checks a tax id (B32.37). Talyvor is the seller of record on the
// marketplace, so it charges the buyer's tax on top of the price; a seller is the supplier of its own supply to
// Talyvor (B32.43). Every rate and registration is data — the tax_jurisdictions, tax_rates and tax_registrations
// rows the operator loads, or a real partner's own (B32.45) — never a literal in the code.
type TaxPartner interface {
	// Name identifies the implementation on every answer it gives.
	Name() string
	// Calculate is the tax on each line of a sale, rounded half-up per line.
	Calculate(ctx context.Context, req TaxRequest) (TaxResult, error)
	// ValidateTaxID checks a VAT or other tax number issued in country. An id that is not valid is an answer,
	// not an error; the error is for a request that cannot be checked at all.
	ValidateTaxID(ctx context.Context, country, id string) (TaxIDResult, error)
}

// SupplierTalyvor is the supplier ID for Talyvor itself: the supplier whose registrations are the
// tax_registrations rows.
const SupplierTalyvor = "talyvor"

// TaxParty is one side of a sale: the supplier, or the customer.
type TaxParty struct {
	ID         string `json:"id"`                    // SupplierTalyvor, or a workspace
	Country    string `json:"country"`               // ISO 3166-1 alpha-2
	Region     string `json:"region,omitempty"`      // a state or province, where tax differs inside a country
	PostalCode string `json:"postal_code,omitempty"` // as the party gave it
	Business   bool   `json:"business"`              // a business, not a consumer
	TaxID      string `json:"tax_id,omitempty"`      // as ValidateTaxID normalised it
	TaxIDValid bool   `json:"tax_id_valid"`          // ValidateTaxID found it valid
}

// TaxLine is one thing sold: its reference, what kind of supply it is, and its price before tax.
type TaxLine struct {
	Ref          string `json:"ref"`           // the caller's reference, such as a market_uses id
	TaxCode      string `json:"tax_code"`      // such as digital_service
	AmountMicros int64  `json:"amount_micros"` // the price before tax, in millionths of Currency (µUSD on the marketplace)
	Currency     string `json:"currency"`
}

// TaxRequest is a sale to work the tax out on: who supplies it, who buys it, what, and when.
type TaxRequest struct {
	Supplier TaxParty  `json:"supplier"`
	Customer TaxParty  `json:"customer"`
	Lines    []TaxLine `json:"lines"`
	At       time.Time `json:"at"` // when the sale is made: the rates and registrations in force then apply
}

// TaxTreatment is how a line is taxed.
type TaxTreatment string

// The treatments.
const (
	// TaxStandard: the supplier charges the jurisdiction's rate.
	TaxStandard TaxTreatment = "standard"
	// TaxReverseCharge: a business customer in another country accounts for the tax itself; none is charged.
	TaxReverseCharge TaxTreatment = "reverse_charge"
	// TaxZero: the supply is taxed at a rate of zero.
	TaxZero TaxTreatment = "zero"
	// TaxOutsideScope: no tax the partner knows of applies to the supply. A real partner's answer (B32.45); the
	// Test partner does not give it.
	TaxOutsideScope TaxTreatment = "outside_scope"
	// TaxNotRegistered: the supplier holds no registration covering the customer's jurisdiction, so charges none.
	TaxNotRegistered TaxTreatment = "not_registered"
	// TaxNoRate: tax is due, but no rate is loaded for the supply in the customer's jurisdiction at that time.
	TaxNoRate TaxTreatment = "no_rate"
)

// TaxLineResult is the tax on one line.
type TaxLineResult struct {
	Ref           string       `json:"ref"`
	Jurisdiction  string       `json:"jurisdiction"`
	RateBps       int          `json:"rate_bps"`       // 100 is 1%
	TaxableMicros int64        `json:"taxable_micros"` // the base the tax is on; 0 where none applies
	TaxMicros     int64        `json:"tax_micros"`     // rounded half-up
	Currency      string       `json:"currency"`
	Treatment     TaxTreatment `json:"treatment"`
	Note          string       `json:"note"` // what a receipt prints for the line
}

// TaxResult is the tax on a sale, line by line, and its totals.
type TaxResult struct {
	Partner       string          `json:"partner"`
	Lines         []TaxLineResult `json:"lines"`
	TaxableMicros int64           `json:"taxable_micros"`
	TaxMicros     int64           `json:"tax_micros"`
}

// TaxIDResult is what a tax id check found.
type TaxIDResult struct {
	Partner string `json:"partner"`
	Country string `json:"country"`
	Number  string `json:"number"` // normalised, with the country's VAT prefix: GB123456789, EL123456789
	Valid   bool   `json:"valid"`
	Detail  string `json:"detail,omitempty"` // why it is not valid
}

// TaxRate is a tax_rates row.
type TaxRate struct {
	Jurisdiction string     `json:"jurisdiction"`
	TaxCode      string     `json:"tax_code"`
	RateBps      int        `json:"rate_bps"`
	ValidFrom    time.Time  `json:"valid_from"`
	ValidTo      *time.Time `json:"valid_to,omitempty"`
	Source       string     `json:"source"`
}

// TaxRegistration is a tax_registrations row: one of Talyvor's own registrations.
type TaxRegistration struct {
	Jurisdiction  string    `json:"jurisdiction"` // a jurisdiction's code, or a union's (EU)
	Scheme        string    `json:"scheme"`       // such as GB VAT, EU OSS non-Union
	Number        string    `json:"number"`
	EffectiveFrom time.Time `json:"effective_from"`
}

// TaxData is where the Test tax partner reads the rates and registrations from: TaxStore over Postgres.
type TaxData interface {
	// TaxRate is the rate for taxCode in jurisdiction at a time, if one is loaded.
	TaxRate(ctx context.Context, jurisdiction, taxCode string, at time.Time) (TaxRate, bool, error)
	// TaxRegistration is Talyvor's registration covering jurisdiction at a time — for the jurisdiction itself or
	// for its union — if it holds one.
	TaxRegistration(ctx context.Context, jurisdiction string, at time.Time) (TaxRegistration, bool, error)
}

// TestTaxPartner is test mode for tax: it decides from the rows in its TaxData and asks no tax authority.
//
//   - A business customer with a valid tax id, in another country than the supplier: reverse_charge, no tax.
//   - Otherwise, the customer's country is the jurisdiction. Unless the supplier is registered there — Talyvor
//     by one of its tax_registrations, any supplier by a valid tax id of that country — not_registered, no tax.
//   - Registered, the jurisdiction's rate for the line's tax code: none loaded is no_rate; 0 is zero; any
//     other is standard, the tax rounded half-up per line.
//
// A business customer without a valid tax id is taxed as a consumer.
//
// ValidateTaxID checks the number's format for its country (GB, XI and the EU member states), and that it is not
// on the fixture list of numbers no authority issued (testdata/tax/invalid_tax_ids.txt). It calls neither VIES
// nor HMRC.
type TestTaxPartner struct {
	Data TaxData
}

// Name is "test".
func (*TestTaxPartner) Name() string { return "test" }

// Calculate is the tax on each line, from the rows in p.Data.
func (p *TestTaxPartner) Calculate(ctx context.Context, req TaxRequest) (TaxResult, error) {
	if err := checkTaxRequest(req); err != nil {
		return TaxResult{}, err
	}
	if p.Data == nil {
		return TaxResult{}, fmt.Errorf("%w: the Test tax partner has no rates to read", ErrInvalid)
	}
	supplier, customer := req.Supplier, req.Customer
	jurisdiction := strings.ToUpper(customer.Country)
	out := TaxResult{Partner: p.Name()}
	for _, l := range req.Lines {
		r := TaxLineResult{Ref: l.Ref, Jurisdiction: jurisdiction, Currency: l.Currency}
		switch {
		case customer.Business && customer.TaxIDValid && !strings.EqualFold(customer.Country, supplier.Country):
			r.Treatment, r.TaxableMicros = TaxReverseCharge, l.AmountMicros
		default:
			registered, err := p.registered(ctx, supplier, jurisdiction, req.At)
			if err != nil {
				return TaxResult{}, err
			}
			if !registered {
				r.Treatment = TaxNotRegistered
				break
			}
			rate, ok, err := p.Data.TaxRate(ctx, jurisdiction, l.TaxCode, req.At)
			if err != nil {
				return TaxResult{}, fmt.Errorf("partners: tax rate for %s in %s: %w", l.TaxCode, jurisdiction, err)
			}
			if !ok {
				r.Treatment = TaxNoRate
				break
			}
			r.RateBps, r.TaxableMicros = rate.RateBps, l.AmountMicros
			r.TaxMicros = taxHalfUp(l.AmountMicros, rate.RateBps)
			if rate.RateBps == 0 {
				r.Treatment = TaxZero
			} else {
				r.Treatment = TaxStandard
			}
		}
		r.Note = taxNote(r.Treatment, jurisdiction, l.TaxCode, r.RateBps)
		out.Lines = append(out.Lines, r)
		out.TaxableMicros += r.TaxableMicros
		out.TaxMicros += r.TaxMicros
	}
	return out, nil
}

// registered says whether the supplier may charge tax in jurisdiction: a valid tax id of that country, or, for
// Talyvor, a registration covering it.
func (p *TestTaxPartner) registered(ctx context.Context, supplier TaxParty, jurisdiction string, at time.Time) (bool, error) {
	if supplier.TaxIDValid && strings.EqualFold(supplier.Country, jurisdiction) {
		return true, nil
	}
	if supplier.ID != SupplierTalyvor {
		return false, nil
	}
	_, ok, err := p.Data.TaxRegistration(ctx, jurisdiction, at)
	if err != nil {
		return false, fmt.Errorf("partners: tax registration in %s: %w", jurisdiction, err)
	}
	return ok, nil
}

var countryCode = regexp.MustCompile(`^[A-Za-z]{2}$`)

func checkTaxRequest(req TaxRequest) error {
	switch {
	case strings.TrimSpace(req.Supplier.ID) == "":
		return fmt.Errorf("%w: a tax request names its supplier", ErrInvalid)
	case !countryCode.MatchString(req.Supplier.Country):
		return fmt.Errorf("%w: the supplier's country is two letters, not %q", ErrInvalid, req.Supplier.Country)
	case !countryCode.MatchString(req.Customer.Country):
		return fmt.Errorf("%w: the customer's country is two letters, not %q", ErrInvalid, req.Customer.Country)
	case len(req.Lines) == 0:
		return fmt.Errorf("%w: a tax request has a line", ErrInvalid)
	case req.At.IsZero():
		return fmt.Errorf("%w: a tax request says when the sale is made", ErrInvalid)
	}
	for _, l := range req.Lines {
		switch {
		case strings.TrimSpace(l.Ref) == "":
			return fmt.Errorf("%w: every tax line carries the caller's reference", ErrInvalid)
		case strings.TrimSpace(l.TaxCode) == "":
			return fmt.Errorf("%w: tax line %s has no tax code", ErrInvalid, l.Ref)
		}
		if _, ok := currencies[l.Currency]; !ok {
			return fmt.Errorf("%w: tax line %s: a currency is GBP, EUR, USD or USDC, not %q", ErrInvalid, l.Ref, l.Currency)
		}
		if l.AmountMicros <= 0 {
			return fmt.Errorf("%w: tax line %s: an amount is positive, not %d", ErrInvalid, l.Ref, l.AmountMicros)
		}
	}
	return nil
}

// taxHalfUp is amount × bps / 10000, rounded half-up. A rate is at most 10000 bps, so the tax fits as the
// amount does.
func taxHalfUp(amount int64, bps int) int64 {
	n := new(big.Int).Mul(big.NewInt(amount), big.NewInt(int64(bps)))
	n.Add(n, big.NewInt(5000))
	return n.Quo(n, big.NewInt(10000)).Int64()
}

// taxNote is what a receipt prints for a line: the same words whichever partner decided the treatment.
func taxNote(t TaxTreatment, jurisdiction, taxCode string, bps int) string {
	switch t {
	case TaxReverseCharge:
		return "Reverse charge: the customer accounts for the tax due in " + jurisdiction
	case TaxNotRegistered:
		return "No tax charged: the supplier is not registered for tax in " + jurisdiction
	case TaxNoRate:
		return "No tax charged: no rate for " + taxCode + " in " + jurisdiction
	case TaxOutsideScope:
		return "No tax charged: no tax applies to " + taxCode + " in " + jurisdiction
	case TaxZero:
		return "Zero-rated in " + jurisdiction
	}
	return "Tax at " + formatBps(bps) + " in " + jurisdiction
}

// formatBps is a rate in basis points as a percentage: 100 is "1%", 1250 is "12.5%", 1205 is "12.05%".
func formatBps(bps int) string {
	s := fmt.Sprintf("%d.%02d", bps/100, bps%100)
	s = strings.TrimSuffix(strings.TrimRight(s, "0"), ".")
	return s + "%"
}

// vatPrefix is the prefix a country's VAT numbers carry, where it is not the country's code.
var vatPrefix = map[string]string{"GR": "EL"}

// vatFormats is the shape of a VAT number after its prefix, for each country the Test tax partner checks.
var vatFormats = map[string]*regexp.Regexp{
	"AT": regexp.MustCompile(`^U\d{8}$`),
	"BE": regexp.MustCompile(`^[01]\d{9}$`),
	"BG": regexp.MustCompile(`^\d{9,10}$`),
	"CY": regexp.MustCompile(`^\d{8}[A-Z]$`),
	"CZ": regexp.MustCompile(`^\d{8,10}$`),
	"DE": regexp.MustCompile(`^\d{9}$`),
	"DK": regexp.MustCompile(`^\d{8}$`),
	"EE": regexp.MustCompile(`^\d{9}$`),
	"ES": regexp.MustCompile(`^[A-Z0-9]\d{7}[A-Z0-9]$`),
	"FI": regexp.MustCompile(`^\d{8}$`),
	"FR": regexp.MustCompile(`^[A-HJ-NP-Z0-9]{2}\d{9}$`),
	"GB": regexp.MustCompile(`^(\d{9}|\d{12}|GD[0-4]\d{2}|HA[5-9]\d{2})$`),
	"GR": regexp.MustCompile(`^\d{9}$`),
	"HR": regexp.MustCompile(`^\d{11}$`),
	"HU": regexp.MustCompile(`^\d{8}$`),
	"IE": regexp.MustCompile(`^(\d{7}[A-W][A-I]?|\d[A-Z+*]\d{5}[A-W])$`),
	"IT": regexp.MustCompile(`^\d{11}$`),
	"LT": regexp.MustCompile(`^(\d{9}|\d{12})$`),
	"LU": regexp.MustCompile(`^\d{8}$`),
	"LV": regexp.MustCompile(`^\d{11}$`),
	"MT": regexp.MustCompile(`^\d{8}$`),
	"NL": regexp.MustCompile(`^\d{9}B\d{2}$`),
	"PL": regexp.MustCompile(`^\d{10}$`),
	"PT": regexp.MustCompile(`^\d{9}$`),
	"RO": regexp.MustCompile(`^\d{2,10}$`),
	"SE": regexp.MustCompile(`^\d{12}$`),
	"SI": regexp.MustCompile(`^\d{8}$`),
	"SK": regexp.MustCompile(`^\d{10}$`),
	"XI": regexp.MustCompile(`^(\d{9}|\d{12}|GD[0-4]\d{2}|HA[5-9]\d{2})$`),
}

//go:embed testdata/tax/invalid_tax_ids.txt
var invalidTaxIDsFile []byte

// invalidTaxIDs is the fixture list: well-formed numbers the Test tax partner reports as never issued.
var invalidTaxIDs = func() map[string]bool {
	m := map[string]bool{}
	s := bufio.NewScanner(bytes.NewReader(invalidTaxIDsFile))
	for s.Scan() {
		if line := strings.TrimSpace(s.Text()); line != "" && !strings.HasPrefix(line, "#") {
			m[line] = true
		}
	}
	return m
}()

// ValidateTaxID checks the number's format and the fixture list; it asks no tax authority.
func (p *TestTaxPartner) ValidateTaxID(_ context.Context, country, id string) (TaxIDResult, error) {
	country = strings.ToUpper(strings.TrimSpace(country))
	if !countryCode.MatchString(country) {
		return TaxIDResult{}, fmt.Errorf("%w: a country is two letters, not %q", ErrInvalid, country)
	}
	number := strings.Map(func(r rune) rune {
		if r == ' ' || r == '.' || r == '-' {
			return -1
		}
		return r
	}, strings.ToUpper(id))
	if number == "" {
		return TaxIDResult{}, fmt.Errorf("%w: a tax id check has a number to check", ErrInvalid)
	}
	prefix := country
	if v, ok := vatPrefix[country]; ok {
		prefix = v
	}
	number = prefix + strings.TrimPrefix(number, prefix)
	res := TaxIDResult{Partner: p.Name(), Country: country, Number: number}
	format, ok := vatFormats[country]
	switch {
	case !ok:
		res.Detail = "test mode checks VAT numbers of GB, XI and the EU member states only, not " + country
	case !format.MatchString(strings.TrimPrefix(number, prefix)):
		res.Detail = "not the format of a " + country + " VAT number"
	case invalidTaxIDs[number]:
		res.Detail = "test mode: this number is on the list of numbers no authority issued"
	default:
		res.Valid = true
	}
	return res, nil
}
