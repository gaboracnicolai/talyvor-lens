package partners

import (
	"context"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	stripe "github.com/stripe/stripe-go/v81"
)

// StripeTaxAPI is Stripe Tax's calculation API (POST /v1/tax/calculations). *billing.LiveStripe satisfies it.
type StripeTaxAPI interface {
	CalculateTax(ctx context.Context, params *stripe.TaxCalculationParams) (*stripe.TaxCalculation, error)
}

// StripeTaxPartner is the real tax partner (B32.45): Stripe Tax decides each line of Talyvor's own sales from the
// head office, registrations and rates of Talyvor's Stripe account. Lens takes Stripe's rate and treatment for a
// line and works the tax out itself, half-up per line in µUSD as the Test partner does — Stripe counts whole cents,
// and a use can cost less than one. The treatments and receipt notes are B32.37's.
//
// Stripe knows only Talyvor's registrations, and has no tax id check of its own, so a sale another supplier makes
// (a seller's supply to Talyvor, B32.43) and ValidateTaxID go to Rows: the Test partner over the tax rows.
type StripeTaxPartner struct {
	API  StripeTaxAPI
	Rows TaxPartner
}

// stripeTaxCodes is the Stripe tax code of each of Lens's tax codes. A line whose code is not here is no_rate.
var stripeTaxCodes = map[string]string{
	"digital_service": "txcd_10000000", // General - Electronically Supplied Services
}

// stripeTreatments is the treatment of each of Stripe's taxability reasons.
//
// shortcut: the portion_*, proportionally_rated and taxable_basis_reduced reasons are taxed at the summed rate on
// the whole amount, which they do not reach for an electronically supplied service; map them exactly when a tax
// code they apply to is added to stripeTaxCodes.
var stripeTreatments = map[stripe.TaxCalculationLineItemTaxBreakdownTaxabilityReason]TaxTreatment{
	"standard_rated": TaxStandard, "reduced_rated": TaxStandard, "portion_standard_rated": TaxStandard,
	"portion_reduced_rated": TaxStandard, "portion_product_exempt": TaxStandard, "proportionally_rated": TaxStandard,
	"taxable_basis_reduced": TaxStandard, "zero_rated": TaxZero, "reverse_charge": TaxReverseCharge,
	"not_collecting": TaxNotRegistered, "not_supported": TaxNoRate, "not_subject_to_tax": TaxOutsideScope,
	"product_exempt": TaxOutsideScope, "product_exempt_holiday": TaxOutsideScope, "customer_exempt": TaxOutsideScope,
}

// Name is "stripe".
func (*StripeTaxPartner) Name() string { return "stripe" }

// ValidateTaxID is the Rows partner's check: Stripe Tax has none to call.
func (p *StripeTaxPartner) ValidateTaxID(ctx context.Context, country, id string) (TaxIDResult, error) {
	return p.Rows.ValidateTaxID(ctx, country, id)
}

// Calculate asks Stripe Tax for the rate and treatment of each line Talyvor supplies.
func (p *StripeTaxPartner) Calculate(ctx context.Context, req TaxRequest) (TaxResult, error) {
	if err := checkTaxRequest(req); err != nil {
		return TaxResult{}, err
	}
	if req.Supplier.ID != SupplierTalyvor {
		return p.Rows.Calculate(ctx, req)
	}
	customer := req.Customer
	country := strings.ToUpper(customer.Country)
	params := &stripe.TaxCalculationParams{
		Currency: stripe.String(strings.ToLower(strings.TrimSuffix(req.Lines[0].Currency, "C"))), // USDC is calculated as USD
		CustomerDetails: &stripe.TaxCalculationCustomerDetailsParams{
			Address:       &stripe.AddressParams{Country: stripe.String(country)},
			AddressSource: stripe.String("billing"),
		},
		TaxDate: stripe.Int64(req.At.Unix()),
	}
	params.AddExpand("line_items")
	params.AddExpand("line_items.data.tax_breakdown")
	if customer.Region != "" {
		params.CustomerDetails.Address.State = stripe.String(customer.Region)
	}
	if customer.PostalCode != "" {
		params.CustomerDetails.Address.PostalCode = stripe.String(customer.PostalCode)
	}
	// A business with a valid VAT number is one to Stripe as well; without one it is a consumer, as to the Test partner.
	if _, vat := vatFormats[country]; vat && customer.Business && customer.TaxIDValid {
		kind := "eu_vat"
		if country == "GB" {
			kind = "gb_vat"
		}
		params.CustomerDetails.TaxIDs = []*stripe.TaxCalculationCustomerDetailsTaxIDParams{{Type: stripe.String(kind), Value: stripe.String(customer.TaxID)}}
	}
	for i, l := range req.Lines {
		if l.Currency != req.Lines[0].Currency {
			// shortcut: one calculation is in one currency; split the request per currency when a caller mixes them.
			return TaxResult{}, fmt.Errorf("%w: Stripe Tax calculates one currency at a time, not %s and %s", ErrInvalid, req.Lines[0].Currency, l.Currency)
		}
		code, ok := stripeTaxCodes[l.TaxCode]
		if !ok {
			continue
		}
		// Stripe takes whole cents, at least one; the rate it answers does not depend on the amount.
		cents := max(1, (l.AmountMicros+5_000)/10_000)
		params.LineItems = append(params.LineItems, &stripe.TaxCalculationLineItemParams{Amount: stripe.Int64(cents),
			Reference: stripe.String(strconv.Itoa(i)), TaxCode: stripe.String(code), TaxBehavior: stripe.String("exclusive")})
	}
	answered := map[string]*stripe.TaxCalculationLineItem{}
	if len(params.LineItems) > 0 {
		calc, err := p.API.CalculateTax(ctx, params)
		if err != nil {
			return TaxResult{}, fmt.Errorf("partners: Stripe Tax: %w", err)
		}
		if calc.LineItems != nil {
			for _, li := range calc.LineItems.Data {
				answered[li.Reference] = li
			}
		}
	}
	out := TaxResult{Partner: p.Name()}
	for i, l := range req.Lines {
		r := TaxLineResult{Ref: l.Ref, Jurisdiction: country, Currency: l.Currency, Treatment: TaxNoRate}
		if _, ok := stripeTaxCodes[l.TaxCode]; ok {
			li := answered[strconv.Itoa(i)]
			if li == nil || len(li.TaxBreakdown) == 0 {
				// Never read a missing breakdown as no tax.
				return TaxResult{}, fmt.Errorf("partners: Stripe Tax answered no tax breakdown for line %s", l.Ref)
			}
			var err error
			if r, err = stripeTaxLine(r, l.AmountMicros, li.TaxBreakdown); err != nil {
				return TaxResult{}, fmt.Errorf("partners: Stripe Tax, line %s: %w", l.Ref, err)
			}
		}
		r.Note = taxNote(r.Treatment, r.Jurisdiction, l.TaxCode, r.RateBps)
		out.Lines = append(out.Lines, r)
		out.TaxableMicros += r.TaxableMicros
		out.TaxMicros += r.TaxMicros
	}
	return out, nil
}

// stripeTaxLine is r decided by a line's tax breakdown: its first entry's reason and jurisdiction, and the sum of the
// rates of the entries that charge one (a US state's, county's and city's).
func stripeTaxLine(r TaxLineResult, amount int64, breakdown []*stripe.TaxCalculationLineItemTaxBreakdown) (TaxLineResult, error) {
	pct := new(big.Rat)
	for i, b := range breakdown {
		t, ok := stripeTreatments[b.TaxabilityReason]
		if !ok {
			return r, fmt.Errorf("a taxability reason Lens does not know: %q", b.TaxabilityReason)
		}
		if i == 0 {
			r.Treatment = t
			if b.Jurisdiction != nil && b.Jurisdiction.Country != "" {
				r.Jurisdiction = b.Jurisdiction.Country
			}
		}
		if (t == TaxStandard || t == TaxZero) && b.TaxRateDetails != nil {
			x, ok := new(big.Rat).SetString(b.TaxRateDetails.PercentageDecimal)
			if !ok {
				return r, fmt.Errorf("a rate that is not a number: %q", b.TaxRateDetails.PercentageDecimal)
			}
			pct.Add(pct, x)
		}
	}
	switch r.Treatment {
	case TaxStandard, TaxZero:
		// The percentage in basis points, half-up: "20.0" is 2000, "8.875" is 888.
		bps := new(big.Rat).Mul(pct, big.NewRat(100, 1))
		n := new(big.Int).Add(new(big.Int).Mul(bps.Num(), big.NewInt(2)), bps.Denom())
		r.RateBps = int(n.Quo(n, new(big.Int).Mul(bps.Denom(), big.NewInt(2))).Int64())
		r.TaxableMicros, r.TaxMicros = amount, taxHalfUp(amount, r.RateBps)
		if r.Treatment = TaxStandard; r.RateBps == 0 {
			r.Treatment = TaxZero
		}
	case TaxReverseCharge:
		r.TaxableMicros = amount
	}
	return r, nil
}
