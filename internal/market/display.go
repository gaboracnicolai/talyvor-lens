package market

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/ecbrate"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/taxprofile"
)

// display.go — B32.51: PRICES IN THE BUYER'S CURRENCY, TAX-INCLUSIVE FOR CONSUMERS.
//
// A listing is priced and charged in US dollars. Every listing and offer read also shows each offer's price in the
// buyer's currency — the one asked for (?currency=), or by default that of the country the buyer's tax profile resolves
// to (USD where it is unknown) — at the ECB reference rate, rounded half-up to the currency's minor unit. A consumer
// Talyvor charges VAT sees it included, as the tax partner works it out for that buyer ("incl. VAT"); a business sees
// the price before it ("+ VAT"). The US-dollar price beside it is unchanged, the charge stays in US dollars on the
// monthly marketplace bill, and each priced listing's price_note says so.

// DisplayRates reads the day's rate from US dollars into a currency: *ecbrate.Book.
type DisplayRates interface {
	USDRate(ctx context.Context, currency string, at time.Time) (ecbrate.USDRate, error)
}

// SetDisplayRates turns on prices in the buyer's currency.
func (s *Store) SetDisplayRates(r DisplayRates) { s.displayRates = r }

// The labels a displayed price carries.
const (
	TaxLabelIncluded = "incl. VAT" // a consumer's price, with the VAT Talyvor charges in it
	TaxLabelAdded    = "+ VAT"     // a business's price, before VAT
)

// Display is an offer's price in the buyer's currency: what it shows; the charge is the offer's price_usd_micros.
type Display struct {
	Currency    string     `json:"currency"`
	AmountMinor int64      `json:"amount_minor"` // rounded half-up to the currency's minor unit
	IncludesTax bool       `json:"includes_tax"`
	TaxLabel    string     `json:"tax_label,omitempty"` // TaxLabelIncluded, TaxLabelAdded, or none
	Rate        string     `json:"rate"`                // units of the currency per US dollar; "1" for USD
	RateDate    *time.Time `json:"rate_date,omitempty"` // the ECB day the rate is from; nil for USD
	Source      string     `json:"source"`              // "ecb", or "none" for USD
}

// chargedInUSD is what every priced listing's price_note starts with.
const chargedInUSD = "Charged in US dollars on your monthly marketplace bill"

var currencyCode = regexp.MustCompile(`^[A-Z]{3}$`)

// ShowPrices fills in the Display of every offer of listings as buyer ("" when the reader is no workspace) sees it, in
// currency ("" for the currency of the buyer's tax-profile country), and each priced listing's PriceNote. Where the ECB
// publishes no rate for the currency the offers carry no Display and the note says why.
func (s *Store) ShowPrices(ctx context.Context, buyer, currency string, listings ...*Listing) error {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if currency != "" && !currencyCode.MatchString(currency) {
		return invalid("currency must be a three-letter ISO 4217 code, such as GBP")
	}
	if s.displayRates == nil {
		return nil
	}
	var offers []*Offer
	for _, l := range listings {
		for i := range l.Offers {
			offers = append(offers, &l.Offers[i])
		}
	}
	if len(offers) == 0 {
		return nil
	}
	who := taxprofile.Resolution{WorkspaceID: buyer}
	if s.tax != nil && buyer != "" {
		r, err := s.tax.Buyers.Resolve(ctx, buyer)
		if err != nil {
			// A price shown is a guide, never a charge: without the buyer's location it is shown without tax.
			slog.Warn("market: prices are shown without the buyer's tax: where the buyer is is not known", "buyer", buyer, "error", err)
		} else {
			who = r
		}
	}
	if currency == "" {
		var err error
		if currency, err = s.countryCurrency(ctx, who); err != nil {
			return err
		}
	}
	rate, err := s.displayRates.USDRate(ctx, currency, s.now())
	if errors.Is(err, ecbrate.ErrNoRate) {
		note := chargedInUSD + "; no ECB reference rate for " + currency + " is published yet."
		for _, l := range listings {
			if len(l.Offers) > 0 {
				l.PriceNote = note
			}
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("market: the rate into %s: %w", currency, err)
	}
	taxes, err := s.consumerTax(ctx, who, offers)
	if err != nil {
		return err
	}
	for i, o := range offers {
		d := Display{}
		gross := o.PriceUSDMicros
		if t, ok := taxes[i]; ok {
			gross += t
			d.IncludesTax, d.TaxLabel = true, TaxLabelIncluded
		} else if who.Known && who.Business {
			d.TaxLabel = TaxLabelAdded
		}
		c := rate.Convert(gross)
		d.Currency, d.AmountMinor, d.Rate, d.RateDate, d.Source = c.Currency, c.AmountMinor, c.Rate, c.RateDate, c.Source
		o.Display = &d
	}
	note := chargedInUSD + "."
	if day := rate.Date(); day != nil {
		note = fmt.Sprintf("%s; %s prices are at the ECB reference rate of %s.", chargedInUSD, currency, day.Format("2 January 2006"))
	}
	for _, l := range listings {
		if len(l.Offers) > 0 {
			l.PriceNote = note
		}
	}
	return nil
}

// countryCurrency is the currency of the country the buyer resolves to, as the operator's tax data names it; USD when
// the buyer's location, or its country's currency, is not known.
func (s *Store) countryCurrency(ctx context.Context, who taxprofile.Resolution) (string, error) {
	if !who.Known {
		return "USD", nil
	}
	var currency string
	err := s.pool.QueryRow(ctx, `SELECT currency FROM tax_jurisdictions WHERE code = $1`, who.Country).Scan(&currency)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && currency == "") {
		return "USD", nil
	}
	if err != nil {
		return "", fmt.Errorf("market: the currency of %s: %w", who.Country, err)
	}
	return strings.ToUpper(currency), nil
}

// consumerTax is, for a consumer Talyvor charges tax, the tax in µUSD on each priced offer, by its index in offers, as
// the tax partner works it out for that buyer now. Nothing for a business, a buyer no evidence places, or when tax is
// not turned on.
func (s *Store) consumerTax(ctx context.Context, who taxprofile.Resolution, offers []*Offer) (map[int]int64, error) {
	if s.tax == nil || !who.Known || who.Business {
		return nil, nil
	}
	var lines []partners.TaxLine
	for i, o := range offers {
		if o.PriceUSDMicros > 0 {
			lines = append(lines, partners.TaxLine{Ref: strconv.Itoa(i), TaxCode: TaxCodeDigitalService, AmountMicros: o.PriceUSDMicros, Currency: "USD"})
		}
	}
	if len(lines) == 0 {
		return nil, nil
	}
	res, err := s.tax.Partners.Tax().Calculate(ctx, partners.TaxRequest{
		Supplier: partners.TaxParty{ID: partners.SupplierTalyvor, Country: SupplierCountry},
		Customer: who.Customer(),
		Lines:    lines,
		At:       s.now(),
	})
	if err != nil {
		return nil, fmt.Errorf("market: the tax on the prices shown: %w", err)
	}
	taxes := map[int]int64{}
	for _, x := range res.Lines {
		if x.Treatment != partners.TaxStandard && x.Treatment != partners.TaxZero {
			continue // Talyvor charges this buyer no tax: the price is shown as it is
		}
		i, err := strconv.Atoi(x.Ref)
		if err != nil || i < 0 || i >= len(offers) {
			return nil, fmt.Errorf("market: the %s tax partner answered a line %q no offer asked for", res.Partner, x.Ref)
		}
		taxes[i] = x.TaxMicros
	}
	return taxes, nil
}
