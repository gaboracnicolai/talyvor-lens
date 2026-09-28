// Package ecbrate keeps the European Central Bank's euro foreign exchange reference rates (B19.12).
//
// The ECB publishes, around 16:00 CET on each TARGET working day, how many units of each currency one
// euro buys. Lens fetches the daily file, keeps every day it has seen in ecb_reference_rates (migration
// 0156), and prices a card authorisation at "the day's" rate: the latest one published on or before its
// date. Any currency's USD value follows from two of them: USD per unit = (USD per EUR) ÷ (unit per EUR).
package ecbrate

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DailyURL is the ECB's file of the latest reference rates.
const DailyURL = "https://www.ecb.europa.eu/stats/eurofxref/eurofxref-daily.xml"

// ErrNoRate: no reference rate for the currency on or before the date.
var ErrNoRate = errors.New("ecbrate: no ECB reference rate for this currency on or before this date")

// Day is one day's published rates: each currency per 1 EUR, as the decimal the ECB printed.
type Day struct {
	Date   time.Time
	PerEUR map[string]string // upper-case ISO code → decimal string
}

type envelope struct {
	Cube struct {
		Days []struct {
			Time  string `xml:"time,attr"`
			Rates []struct {
				Currency string `xml:"currency,attr"`
				Rate     string `xml:"rate,attr"`
			} `xml:"Cube"`
		} `xml:"Cube"`
	} `xml:"Cube"`
}

// Parse reads the ECB's eurofxref XML (the daily file, or the 90-day one) into its days.
func Parse(r io.Reader) ([]Day, error) {
	var env envelope
	if err := xml.NewDecoder(r).Decode(&env); err != nil {
		return nil, fmt.Errorf("ecbrate: parse: %w", err)
	}
	var days []Day
	for _, d := range env.Cube.Days {
		date, err := time.Parse("2006-01-02", d.Time)
		if err != nil {
			return nil, fmt.Errorf("ecbrate: date %q: %w", d.Time, err)
		}
		day := Day{Date: date, PerEUR: map[string]string{}}
		for _, r := range d.Rates {
			rat, ok := new(big.Rat).SetString(r.Rate)
			if !ok || rat.Sign() <= 0 {
				return nil, fmt.Errorf("ecbrate: %s rate %q is not a positive decimal", r.Currency, r.Rate)
			}
			day.PerEUR[strings.ToUpper(r.Currency)] = r.Rate
		}
		days = append(days, day)
	}
	if len(days) == 0 {
		return nil, errors.New("ecbrate: the file holds no rates")
	}
	return days, nil
}

// Book keeps the rates in Postgres and fetches them from the ECB.
type Book struct {
	pool   *pgxpool.Pool
	client *http.Client
	url    string
}

// New keeps rates in pool, fetched from url (DailyURL in production).
func New(pool *pgxpool.Pool, url string) *Book {
	return &Book{pool: pool, client: &http.Client{Timeout: 10 * time.Second}, url: url}
}

// Refresh fetches the ECB's file and stores every day in it; a day already stored is left as it is.
func (b *Book) Refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.url, nil)
	if err != nil {
		return err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("ecbrate: fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ecbrate: fetch: %s", resp.Status)
	}
	days, err := Parse(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	for _, d := range days {
		for ccy, rate := range d.PerEUR {
			if _, err := b.pool.Exec(ctx, `INSERT INTO ecb_reference_rates (rate_date, currency, per_eur) VALUES ($1, $2, $3::numeric)
				ON CONFLICT (rate_date, currency) DO NOTHING`, d.Date, ccy, rate); err != nil {
				return fmt.Errorf("ecbrate: store: %w", err)
			}
		}
	}
	return nil
}

// Conversion is an amount's USD value at the day's reference rate, and the rate it was priced at.
type Conversion struct {
	RateDate       *time.Time // nil for USD, which needs no rate
	USDPerEUR      string     // as the ECB printed it; "" for USD
	CurrencyPerEUR string     // "1" for EUR; "" for USD
	USDMicros      int64      // rounded up to the micro-dollar
}

// perEUR is the rate of ccy on the latest date on or before `on` that has one.
func (b *Book) perEUR(ctx context.Context, ccy string, on time.Time) (time.Time, string, error) {
	var date time.Time
	var rate string
	err := b.pool.QueryRow(ctx, `SELECT rate_date, per_eur::text FROM ecb_reference_rates
		WHERE currency = $1 AND rate_date <= $2 ORDER BY rate_date DESC LIMIT 1`, ccy, on).Scan(&date, &rate)
	if errors.Is(err, pgx.ErrNoRows) {
		return date, "", ErrNoRate
	}
	return date, rate, err
}

// ToUSD prices amountMinor (the smallest unit of a two-decimal currency: usd, eur or gbp) in USD at the
// reference rate of the day `at` falls on (UTC). USD and EUR must come from the same ECB day, so the
// currency's rate fixes the date and USD's is read on it.
func (b *Book) ToUSD(ctx context.Context, amountMinor int64, currency string, at time.Time) (Conversion, error) {
	ccy := strings.ToUpper(currency)
	switch ccy {
	case "USD":
		return Conversion{USDMicros: amountMinor * 10_000}, nil
	case "EUR", "GBP":
	default:
		return Conversion{}, fmt.Errorf("ecbrate: cannot price %s", currency)
	}
	on := at.UTC()
	date, usd, err := b.perEUR(ctx, "USD", on)
	if err != nil {
		return Conversion{}, err
	}
	c := Conversion{RateDate: &date, USDPerEUR: usd, CurrencyPerEUR: "1"}
	if ccy != "EUR" {
		var d time.Time
		if d, c.CurrencyPerEUR, err = b.perEUR(ctx, ccy, date); err != nil {
			return Conversion{}, err
		}
		if !d.Equal(date) {
			return Conversion{}, fmt.Errorf("%w: the ECB's %s rate for %s is missing", ErrNoRate, ccy, date.Format("2006-01-02"))
		}
	}
	c.USDMicros = USDMicros(amountMinor, c.USDPerEUR, c.CurrencyPerEUR)
	return c, nil
}

// USDMicros is ceil(amountMinor ÷ 100 × 10⁶ × usdPerEUR ÷ ccyPerEUR), exactly.
func USDMicros(amountMinor int64, usdPerEUR, ccyPerEUR string) int64 {
	usd, _ := new(big.Rat).SetString(usdPerEUR)
	ccy, _ := new(big.Rat).SetString(ccyPerEUR)
	v := new(big.Rat).SetInt64(amountMinor * 10_000)
	v.Mul(v, usd).Quo(v, ccy)
	q, r := new(big.Int).QuoRem(v.Num(), v.Denom(), new(big.Int))
	if r.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return q.Int64()
}
