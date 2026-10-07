package partners

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TaxStore is the tax rows in Postgres — tax_jurisdictions, tax_rates and tax_registrations (0222) — as the
// operator loads them, and the TaxData the Test tax partner reads.
type TaxStore struct {
	pool *pgxpool.Pool
}

// NewTaxStore is the tax rows in pool.
func NewTaxStore(pool *pgxpool.Pool) *TaxStore { return &TaxStore{pool: pool} }

// TaxRate is the rate for taxCode in jurisdiction at a time: the latest that started by then and has not ended.
func (s *TaxStore) TaxRate(ctx context.Context, jurisdiction, taxCode string, at time.Time) (TaxRate, bool, error) {
	var r TaxRate
	err := s.pool.QueryRow(ctx, `SELECT jurisdiction, tax_code, rate_bps, valid_from, valid_to, source FROM tax_rates
		WHERE jurisdiction = $1 AND tax_code = $2 AND valid_from <= $3 AND (valid_to IS NULL OR valid_to > $3)
		ORDER BY valid_from DESC LIMIT 1`, jurisdiction, taxCode, at).
		Scan(&r.Jurisdiction, &r.TaxCode, &r.RateBps, &r.ValidFrom, &r.ValidTo, &r.Source)
	if errors.Is(err, pgx.ErrNoRows) {
		return TaxRate{}, false, nil
	}
	return r, err == nil, err
}

// TaxRegistration is Talyvor's registration covering jurisdiction at a time: its own before its union's.
func (s *TaxStore) TaxRegistration(ctx context.Context, jurisdiction string, at time.Time) (TaxRegistration, bool, error) {
	var r TaxRegistration
	err := s.pool.QueryRow(ctx, `SELECT jurisdiction, scheme, number, effective_from FROM tax_registrations
		WHERE effective_from <= $2
		  AND (jurisdiction = $1 OR jurisdiction = (SELECT union_code FROM tax_jurisdictions WHERE code = $1))
		ORDER BY jurisdiction = $1 DESC, effective_from DESC LIMIT 1`, jurisdiction, at).
		Scan(&r.Jurisdiction, &r.Scheme, &r.Number, &r.EffectiveFrom)
	if errors.Is(err, pgx.ErrNoRows) {
		return TaxRegistration{}, false, nil
	}
	return r, err == nil, err
}

// TaxRateRow is one line of a rates file: a rate, and the jurisdiction it is in.
type TaxRateRow struct {
	TaxRate
	Currency                  string // the jurisdiction's
	RateSource                string // where its tax is converted to Currency, such as ecb
	RegistrationFromFirstSale bool
	Union                     string // the union whose one-stop registration covers it, such as EU; may be empty
}

// TaxRatesCSVHeader is the header a rates file starts with, in any order. valid_to and union may be empty;
// dates are YYYY-MM-DD, UTC.
const TaxRatesCSVHeader = "jurisdiction,currency,rate_source,registration_from_first_sale,union,tax_code,rate_bps,valid_from,valid_to,source"

// ParseTaxRatesCSV reads a rates file. It refuses the whole file at its first bad line.
func ParseTaxRatesCSV(r io.Reader) ([]TaxRateRow, error) {
	cr := csv.NewReader(r)
	cr.Comment = '#'
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("tax rates: the header: %w", err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.TrimSpace(h)] = i
	}
	for _, want := range strings.Split(TaxRatesCSVHeader, ",") {
		if _, ok := col[want]; !ok {
			return nil, fmt.Errorf("tax rates: the header has no %s column; it is %s", want, TaxRatesCSVHeader)
		}
	}
	var rows []TaxRateRow
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return rows, nil
		}
		if err != nil {
			return nil, fmt.Errorf("tax rates: line %d: %w", line, err)
		}
		get := func(name string) string { return strings.TrimSpace(rec[col[name]]) }
		row := TaxRateRow{TaxRate: TaxRate{Jurisdiction: strings.ToUpper(get("jurisdiction")), TaxCode: get("tax_code"),
			Source: get("source")}, Currency: strings.ToUpper(get("currency")), RateSource: get("rate_source"),
			Union: strings.ToUpper(get("union"))}
		if row.RegistrationFromFirstSale, err = strconv.ParseBool(get("registration_from_first_sale")); err != nil {
			return nil, fmt.Errorf("tax rates: line %d: registration_from_first_sale is true or false, not %q", line, get("registration_from_first_sale"))
		}
		if row.RateBps, err = strconv.Atoi(get("rate_bps")); err != nil || row.RateBps < 0 || row.RateBps > 10000 {
			return nil, fmt.Errorf("tax rates: line %d: rate_bps is whole basis points from 0 to 10000 (100 is 1%%), not %q", line, get("rate_bps"))
		}
		if row.ValidFrom, err = time.Parse(time.DateOnly, get("valid_from")); err != nil {
			return nil, fmt.Errorf("tax rates: line %d: valid_from is a day (2026-01-01), not %q", line, get("valid_from"))
		}
		if v := get("valid_to"); v != "" {
			to, err := time.Parse(time.DateOnly, v)
			if err != nil || !to.After(row.ValidFrom) {
				return nil, fmt.Errorf("tax rates: line %d: valid_to is empty or a day after valid_from, not %q", line, v)
			}
			row.ValidTo = &to
		}
		switch {
		case !countryCode.MatchString(row.Jurisdiction):
			return nil, fmt.Errorf("tax rates: line %d: jurisdiction is a two-letter country, not %q", line, row.Jurisdiction)
		case row.Union != "" && !countryCode.MatchString(row.Union):
			return nil, fmt.Errorf("tax rates: line %d: union is empty or two letters, not %q", line, row.Union)
		case len(row.Currency) != 3:
			return nil, fmt.Errorf("tax rates: line %d: currency is a three-letter code, not %q", line, row.Currency)
		case row.RateSource == "" || row.TaxCode == "" || row.Source == "":
			return nil, fmt.Errorf("tax rates: line %d: rate_source, tax_code and source are all filled", line)
		}
		rows = append(rows, row)
	}
}

// ImportRates writes rows in one transaction: each jurisdiction as its last row has it, and each rate, a rate
// already loaded for the same jurisdiction, tax code and start replaced. Importing a file twice changes nothing.
// It answers how many rates and jurisdictions it wrote.
func (s *TaxStore) ImportRates(ctx context.Context, rows []TaxRateRow) (rates, jurisdictions int, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	seen := map[string]bool{}
	for _, r := range rows {
		var union *string
		if r.Union != "" {
			union = &r.Union
		}
		if _, err := tx.Exec(ctx, `INSERT INTO tax_jurisdictions (code, currency, rate_source, registration_from_first_sale, union_code)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (code) DO UPDATE SET currency = EXCLUDED.currency, rate_source = EXCLUDED.rate_source,
				registration_from_first_sale = EXCLUDED.registration_from_first_sale, union_code = EXCLUDED.union_code,
				updated_at = now()`,
			r.Jurisdiction, r.Currency, r.RateSource, r.RegistrationFromFirstSale, union); err != nil {
			return 0, 0, fmt.Errorf("tax rates: jurisdiction %s: %w", r.Jurisdiction, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO tax_rates (jurisdiction, tax_code, rate_bps, valid_from, valid_to, source)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (jurisdiction, tax_code, valid_from) DO UPDATE SET rate_bps = EXCLUDED.rate_bps,
				valid_to = EXCLUDED.valid_to, source = EXCLUDED.source`,
			r.Jurisdiction, r.TaxCode, r.RateBps, r.ValidFrom, r.ValidTo, r.Source); err != nil {
			return 0, 0, fmt.Errorf("tax rates: %s %s from %s: %w", r.Jurisdiction, r.TaxCode, r.ValidFrom.Format(time.DateOnly), err)
		}
		seen[r.Jurisdiction] = true
	}
	return len(rows), len(seen), tx.Commit(ctx)
}

// AddRegistration records one of Talyvor's registrations, replacing one under the same scheme in the same
// jurisdiction. Its jurisdiction is a loaded jurisdiction's code or union: import the rates first.
func (s *TaxStore) AddRegistration(ctx context.Context, r TaxRegistration) error {
	r.Jurisdiction = strings.ToUpper(strings.TrimSpace(r.Jurisdiction))
	switch {
	case !countryCode.MatchString(r.Jurisdiction):
		return fmt.Errorf("%w: a registration's jurisdiction is two letters, not %q", ErrInvalid, r.Jurisdiction)
	case strings.TrimSpace(r.Scheme) == "" || strings.TrimSpace(r.Number) == "":
		return fmt.Errorf("%w: a registration names its scheme and its number", ErrInvalid)
	case r.EffectiveFrom.IsZero():
		return fmt.Errorf("%w: a registration says when it is effective from", ErrInvalid)
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO tax_registrations (jurisdiction, scheme, number, effective_from)
		SELECT $1, $2, $3, $4 WHERE EXISTS (SELECT 1 FROM tax_jurisdictions WHERE code = $1 OR union_code = $1)
		ON CONFLICT (jurisdiction, scheme) DO UPDATE SET number = EXCLUDED.number, effective_from = EXCLUDED.effective_from`,
		r.Jurisdiction, r.Scheme, r.Number, r.EffectiveFrom)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: no jurisdiction or union %s is loaded; import its rates first", ErrInvalid, r.Jurisdiction)
	}
	return nil
}

// Rates is every rate loaded, by jurisdiction, tax code and start.
func (s *TaxStore) Rates(ctx context.Context) ([]TaxRate, error) {
	rows, err := s.pool.Query(ctx, `SELECT jurisdiction, tax_code, rate_bps, valid_from, valid_to, source FROM tax_rates
		ORDER BY jurisdiction, tax_code, valid_from`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaxRate
	for rows.Next() {
		var r TaxRate
		if err := rows.Scan(&r.Jurisdiction, &r.TaxCode, &r.RateBps, &r.ValidFrom, &r.ValidTo, &r.Source); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Registrations is every registration Talyvor holds, by jurisdiction and scheme.
func (s *TaxStore) Registrations(ctx context.Context) ([]TaxRegistration, error) {
	rows, err := s.pool.Query(ctx, `SELECT jurisdiction, scheme, number, effective_from FROM tax_registrations
		ORDER BY jurisdiction, scheme`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaxRegistration
	for rows.Next() {
		var r TaxRegistration
		if err := rows.Scan(&r.Jurisdiction, &r.Scheme, &r.Number, &r.EffectiveFrom); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
