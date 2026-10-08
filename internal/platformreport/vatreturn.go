package platformreport

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// vatreturn.go — the figures for a quarter's VAT return: the UK VAT return (GB) and the EU OSS return (EU, every
// member state, or one of them).
//
// A sale is in the quarter its buyer's invoice was paid — the quarter of its clear entry in the marketplace journal —
// and its figures are its tax line (market_tax_lines, B32.39), summed by jurisdiction, treatment and rate. A refund is
// in the quarter of its reversal entry, and takes its line's figures back. The tax the journal credited the tax:<XX>
// accounts in the quarter is beside it as the check: the two are equal unless a use cleared before its tax was billed.

// ReturnLine is one jurisdiction, treatment and rate's figures in a return.
type ReturnLine struct {
	Jurisdiction             string `json:"jurisdiction"`
	Treatment                string `json:"treatment"`
	RateBps                  int    `json:"rate_bps"`
	Sales                    int64  `json:"sales"`
	TaxableUSDMicros         int64  `json:"taxable_usd_micros"`
	TaxUSDMicros             int64  `json:"tax_usd_micros"`
	Refunds                  int64  `json:"refunds"`
	RefundedTaxableUSDMicros int64  `json:"refunded_taxable_usd_micros"`
	RefundedTaxUSDMicros     int64  `json:"refunded_tax_usd_micros"`
	NetTaxableUSDMicros      int64  `json:"net_taxable_usd_micros"`
	NetTaxUSDMicros          int64  `json:"net_tax_usd_micros"`
}

// Return is a quarter's VAT return figures for a jurisdiction.
type Return struct {
	Jurisdiction string       `json:"jurisdiction"` // GB, a member state, or EU for every member state
	Quarter      string       `json:"quarter"`      // such as 2026Q4
	From         time.Time    `json:"from"`
	To           time.Time    `json:"to"`
	Funding      string       `json:"funding"`
	Currency     string       `json:"currency"` // the figures are µUSD of it
	Lines        []ReturnLine `json:"lines"`
	// The sums of the lines' net figures.
	TaxableUSDMicros int64 `json:"taxable_usd_micros"`
	TaxUSDMicros     int64 `json:"tax_usd_micros"`
	// JournalTaxUSDMicros is what the quarter's clear and reversal entries credited the jurisdictions' tax accounts.
	JournalTaxUSDMicros int64 `json:"journal_tax_usd_micros"`
}

var quarterName = regexp.MustCompile(`^(\d{4})Q([1-4])$`)

// QuarterBounds is the first instant (UTC) of quarter, such as 2026Q4, and of the next.
func QuarterBounds(quarter string) (from, to time.Time, err error) {
	m := quarterName.FindStringSubmatch(quarter)
	if m == nil {
		return from, to, invalid("quarter is a year and a quarter, such as 2026Q4, not %q", quarter)
	}
	year, _ := strconv.Atoi(m[1])
	q, _ := strconv.Atoi(m[2])
	from = time.Date(year, time.Month(3*(q-1)+1), 1, 0, 0, 0, 0, time.UTC)
	return from, from.AddDate(0, 3, 0), nil
}

// TaxReturn reads quarter's figures for jurisdiction — GB, EU or a member state — from funding's money.
func (g *Generator) TaxReturn(ctx context.Context, jurisdiction, quarter, funding string) (Return, error) {
	jurisdiction = strings.ToUpper(strings.TrimSpace(jurisdiction))
	var codes []string
	switch {
	case jurisdiction == "EU":
		codes = memberStates
	case jurisdiction == "GB" || MemberState(jurisdiction):
		codes = []string{jurisdiction}
	default:
		return Return{}, invalid("jurisdiction is GB, EU or an EU member state such as DE, not %q", jurisdiction)
	}
	if err := checkFunding(funding); err != nil {
		return Return{}, err
	}
	from, to, err := QuarterBounds(quarter)
	if err != nil {
		return Return{}, err
	}
	ret := Return{Jurisdiction: jurisdiction, Quarter: quarter, From: from, To: to, Funding: funding, Currency: "USD", Lines: []ReturnLine{}}
	// The entries whose money was funding's: Stripe's collection is posted with the funding of the invoice that paid it.
	rows, err := g.pool.Query(ctx, `SELECT t.jurisdiction, t.treatment, t.rate_bps,
			count(*) FILTER (WHERE j.kind = 'clear'), COALESCE(sum(t.taxable_usd_micros) FILTER (WHERE j.kind = 'clear'), 0)::bigint,
			COALESCE(sum(t.tax_usd_micros) FILTER (WHERE j.kind = 'clear'), 0)::bigint,
			count(*) FILTER (WHERE j.kind = 'reversal'), COALESCE(sum(t.taxable_usd_micros) FILTER (WHERE j.kind = 'reversal'), 0)::bigint,
			COALESCE(sum(t.tax_usd_micros) FILTER (WHERE j.kind = 'reversal'), 0)::bigint
		FROM market_tax_lines t
		JOIN market_journal_entries j ON j.ref = t.use_id AND j.kind IN ('clear', 'reversal')
		WHERE t.jurisdiction = ANY($1) AND j.created_at >= $2 AND j.created_at < $3
		  AND EXISTS (SELECT 1 FROM market_journal_postings p WHERE p.entry_id = j.id AND p.account = 'stripe:clearing' AND p.funding = $4)
		GROUP BY 1, 2, 3 ORDER BY 1, 2, 3`, codes, from, to, funding)
	if err != nil {
		return Return{}, fmt.Errorf("platformreport: tax return: %w", err)
	}
	for rows.Next() {
		var l ReturnLine
		if err := rows.Scan(&l.Jurisdiction, &l.Treatment, &l.RateBps, &l.Sales, &l.TaxableUSDMicros, &l.TaxUSDMicros,
			&l.Refunds, &l.RefundedTaxableUSDMicros, &l.RefundedTaxUSDMicros); err != nil {
			rows.Close()
			return Return{}, fmt.Errorf("platformreport: tax return: %w", err)
		}
		l.NetTaxableUSDMicros = l.TaxableUSDMicros - l.RefundedTaxableUSDMicros
		l.NetTaxUSDMicros = l.TaxUSDMicros - l.RefundedTaxUSDMicros
		ret.TaxableUSDMicros += l.NetTaxableUSDMicros
		ret.TaxUSDMicros += l.NetTaxUSDMicros
		ret.Lines = append(ret.Lines, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Return{}, fmt.Errorf("platformreport: tax return: %w", err)
	}
	accounts := make([]string, len(codes))
	for i, c := range codes {
		accounts[i] = "tax:" + c
	}
	if err := g.pool.QueryRow(ctx, `SELECT COALESCE(-sum(p.amount_usd_micros), 0)::bigint
		FROM market_journal_postings p JOIN market_journal_entries j ON j.id = p.entry_id
		WHERE j.kind IN ('clear', 'reversal') AND p.account = ANY($1) AND p.funding = $4 AND j.created_at >= $2 AND j.created_at < $3`,
		accounts, from, to, funding).Scan(&ret.JournalTaxUSDMicros); err != nil {
		return Return{}, fmt.Errorf("platformreport: tax return: %w", err)
	}
	return ret, nil
}
