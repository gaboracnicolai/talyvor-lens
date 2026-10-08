// Package platformreport is what Talyvor reports, as a platform, about the sellers it pays (B32.44): the annual
// export the UK reporting rules for digital platforms and the EU's DAC7 ask for, and the figures for the UK VAT
// return and the EU OSS return.
//
//   - The export has one record per reportable seller and activity. A seller is reportable when their country of
//     residence (seller_tax_profiles, B32.41) is the UK or an EU member state; a seller credited in the year with no
//     country on file is listed as unresolved, since nobody can yet say whether they are.
//   - A record carries the seller's identification — the TINs, date of birth and financial account identifier opened
//     with envelope.Use (sellertax.Identify) and written into the file only — and, for each quarter of the year, the
//     consideration credited to them, the number of activities it was credited for, Talyvor's fees on their sales and
//     the taxes withheld (Talyvor withholds none). Every figure is read from the marketplace journal: the clear and
//     reversal entries in the quarter (UTC) that credited the seller's holdback, and the fee postings of the seller's
//     own sales (market_earnings). Amounts are µUSD, the journal's unit.
//   - The activity column says what the money was for — digital_listing for a use of a listing, agent_payment for a
//     payment to another company's agent, and personal_service for a task done by a person once B30.84 ships — so
//     Talyvor's accountant decides which the rules cover.
//   - Each file written is recorded in platform_reports (year, funding, format, generated_at, operator, rows, sha256)
//     and in the operator audit trail, in one transaction. Neither holds a TIN.
//
// docs/platform-reporting.md documents the file.
package platformreport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/operatoraudit"
	"github.com/talyvor/lens/internal/sellertax"
)

// ErrInvalid is a request for a report that cannot be made as asked.
var ErrInvalid = errors.New("platformreport: invalid")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

// The activities a record can be for.
const (
	ActivityDigitalListing = "digital_listing"
	ActivityAgentPayment   = "agent_payment"
)

// The money a report covers.
const (
	FundingLive = "live"
	FundingTest = "test"
)

// The formats a report is written in.
const (
	FormatCSV  = "csv"
	FormatJSON = "json"
)

// AuditAction is the operator audit trail's name for an export.
const AuditAction = "platform_report.export"

// memberStates is the EU's member states, whose residents DAC7 covers.
var memberStates = []string{"AT", "BE", "BG", "CY", "CZ", "DE", "DK", "EE", "ES", "FI", "FR", "GR", "HR", "HU", "IE", "IT",
	"LT", "LU", "LV", "MT", "NL", "PL", "PT", "RO", "SE", "SI", "SK"}

// MemberState is whether country is an EU member state.
func MemberState(country string) bool {
	for _, c := range memberStates {
		if c == country {
			return true
		}
	}
	return false
}

// Reportable is whether a seller resident in country is in the report: the UK or an EU member state.
func Reportable(country string) bool { return country == "GB" || MemberState(country) }

// Quarter is a record's figures for one quarter of the year, or for the whole of it.
type Quarter struct {
	ConsiderationUSDMicros int64 `json:"consideration_usd_micros"`  // credited to the seller, less what was reversed
	Activities             int64 `json:"activities"`                // the sales and royalties it was credited for
	FeesUSDMicros          int64 `json:"fees_usd_micros"`           // Talyvor's fees on the seller's own sales
	TaxesWithheldUSDMicros int64 `json:"taxes_withheld_usd_micros"` // always 0: Talyvor withholds no tax
}

func (q *Quarter) add(o Quarter) {
	q.ConsiderationUSDMicros += o.ConsiderationUSDMicros
	q.Activities += o.Activities
	q.FeesUSDMicros += o.FeesUSDMicros
	q.TaxesWithheldUSDMicros += o.TaxesWithheldUSDMicros
}

// Record is one reportable seller's activity of one kind in the year, with their identification in clear.
type Record struct {
	WorkspaceID               string          `json:"workspace_id"`
	SellerType                string          `json:"seller_type"`
	FirstName                 string          `json:"first_name"`
	MiddleName                string          `json:"middle_name"`
	LastName                  string          `json:"last_name"`
	LegalName                 string          `json:"legal_name"`
	Address                   string          `json:"primary_address"`
	Country                   string          `json:"country_of_residence"`
	TINs                      []sellertax.TIN `json:"tins"`
	DateOfBirth               string          `json:"date_of_birth"`
	CompanyRegistrationNumber string          `json:"company_registration_number"`
	VATNumber                 string          `json:"vat_number"`
	AccountIdentifier         string          `json:"financial_account_identifier"`
	AccountHolder             string          `json:"financial_account_holder"`
	DetailsComplete           bool            `json:"details_complete"`
	Activity                  string          `json:"activity"`
	Quarters                  [4]Quarter      `json:"quarters"`
	Total                     Quarter         `json:"total"`
}

// Report is a year's export.
type Report struct {
	Year        int       `json:"year"`
	Funding     string    `json:"funding"`
	Currency    string    `json:"currency"` // the figures are µUSD of it
	GeneratedAt time.Time `json:"generated_at"`
	Records     []Record  `json:"records"`
	// Unresolved is the sellers credited in the year whose country of residence is not on file.
	Unresolved []string `json:"unresolved_sellers"`
}

// Run is one file written: what platform_reports records of it.
type Run struct {
	ID          string    `json:"id"`
	Year        int       `json:"year"`
	Funding     string    `json:"funding"`
	Format      string    `json:"format"`
	GeneratedAt time.Time `json:"generated_at"`
	Operator    string    `json:"operator"`
	Rows        int       `json:"rows"`
	SHA256      string    `json:"sha256"`
}

// Generator reads the journal and the sellers' details.
type Generator struct {
	pool    *pgxpool.Pool
	sellers *sellertax.Store
	now     func() time.Time
}

// New is a generator over pool, opening the sellers' details through sellers.
func New(pool *pgxpool.Pool, sellers *sellertax.Store) *Generator {
	return &Generator{pool: pool, sellers: sellers, now: time.Now}
}

func checkYear(year int) error {
	if year < 2020 || year > 2100 {
		return invalid("year is a year such as 2026, not %d", year)
	}
	return nil
}

func checkFunding(funding string) error {
	if funding != FundingLive && funding != FundingTest {
		return invalid("funding is %q or %q, not %q", FundingLive, FundingTest, funding)
	}
	return nil
}

type key struct{ ws, activity string }

// Generate reads year's report of funding's money.
func (g *Generator) Generate(ctx context.Context, year int, funding string) (Report, error) {
	if err := checkYear(year); err != nil {
		return Report{}, err
	}
	if err := checkFunding(funding); err != nil {
		return Report{}, err
	}
	from := time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(1, 0, 0)
	rep := Report{Year: year, Funding: funding, Currency: "USD", GeneratedAt: g.now().UTC().Truncate(time.Second),
		Records: []Record{}, Unresolved: []string{}}

	figures := map[key]*[4]Quarter{}
	at := func(ws, activity string) *[4]Quarter {
		k := key{ws, activity}
		if figures[k] == nil {
			figures[k] = &[4]Quarter{}
		}
		return figures[k]
	}
	// What each clear and reversal in the year credited, or took back from, each seller's holdback.
	rows, err := g.pool.Query(ctx, `SELECT substring(p.account FROM '^seller:(.+):holdback$'),
			CASE WHEN COALESCE(u.payee_agent_id, '') <> '' THEN 'agent_payment' ELSE 'digital_listing' END,
			extract(quarter FROM j.created_at AT TIME ZONE 'UTC')::int,
			(-sum(p.amount_usd_micros))::bigint, count(DISTINCT j.id) FILTER (WHERE j.kind = 'clear')
		FROM market_journal_postings p
		JOIN market_journal_entries j ON j.id = p.entry_id
		LEFT JOIN market_uses u ON u.id = j.ref
		WHERE j.kind IN ('clear', 'reversal') AND p.funding = $3 AND p.account ~ '^seller:.+:holdback$'
		  AND j.created_at >= $1 AND j.created_at < $2
		GROUP BY 1, 2, 3`, from, to, funding)
	if err != nil {
		return Report{}, fmt.Errorf("platformreport: consideration: %w", err)
	}
	for rows.Next() {
		var ws, activity string
		var q int
		var amount, n int64
		if err := rows.Scan(&ws, &activity, &q, &amount, &n); err != nil {
			rows.Close()
			return Report{}, fmt.Errorf("platformreport: consideration: %w", err)
		}
		f := at(ws, activity)
		f[q-1].ConsiderationUSDMicros += amount
		f[q-1].Activities += n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Report{}, fmt.Errorf("platformreport: consideration: %w", err)
	}
	// Talyvor's fee on each seller's own sales, as the same entries posted it.
	rows, err = g.pool.Query(ctx, `SELECT e.seller_workspace_id,
			CASE WHEN COALESCE(u.payee_agent_id, '') <> '' THEN 'agent_payment' ELSE 'digital_listing' END,
			extract(quarter FROM j.created_at AT TIME ZONE 'UTC')::int, (-sum(p.amount_usd_micros))::bigint
		FROM market_journal_postings p
		JOIN market_journal_entries j ON j.id = p.entry_id
		JOIN market_earnings e ON e.use_id = j.ref AND e.kind = 'sale'
		LEFT JOIN market_uses u ON u.id = j.ref
		WHERE j.kind IN ('clear', 'reversal') AND p.funding = $3 AND p.account = 'revenue:market_fee'
		  AND j.created_at >= $1 AND j.created_at < $2
		GROUP BY 1, 2, 3`, from, to, funding)
	if err != nil {
		return Report{}, fmt.Errorf("platformreport: fees: %w", err)
	}
	for rows.Next() {
		var ws, activity string
		var q int
		var fee int64
		if err := rows.Scan(&ws, &activity, &q, &fee); err != nil {
			rows.Close()
			return Report{}, fmt.Errorf("platformreport: fees: %w", err)
		}
		at(ws, activity)[q-1].FeesUSDMicros += fee
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Report{}, fmt.Errorf("platformreport: fees: %w", err)
	}
	if len(figures) == 0 {
		return rep, nil
	}

	// Where each seller lives decides whether they are reported; only the reportable ones' details are opened.
	sellers := []string{}
	seen := map[string]bool{}
	for k := range figures {
		if !seen[k.ws] {
			seen[k.ws] = true
			sellers = append(sellers, k.ws)
		}
	}
	countries := map[string]string{}
	rows, err = g.pool.Query(ctx, `SELECT workspace_id, country FROM seller_tax_profiles WHERE workspace_id = ANY($1)`, sellers)
	if err != nil {
		return Report{}, fmt.Errorf("platformreport: residence: %w", err)
	}
	for rows.Next() {
		var ws, country string
		if err := rows.Scan(&ws, &country); err != nil {
			rows.Close()
			return Report{}, fmt.Errorf("platformreport: residence: %w", err)
		}
		countries[ws] = country
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Report{}, fmt.Errorf("platformreport: residence: %w", err)
	}
	reportable := []string{}
	for _, ws := range sellers {
		switch c := countries[ws]; {
		case c == "":
			rep.Unresolved = append(rep.Unresolved, ws)
		case Reportable(c):
			reportable = append(reportable, ws)
		}
	}
	sort.Strings(rep.Unresolved)
	if len(reportable) == 0 {
		return rep, nil
	}
	ids, err := g.sellers.Identify(ctx, reportable)
	if err != nil {
		return Report{}, err
	}
	for k, f := range figures {
		id, ok := ids[k.ws]
		if !ok {
			continue
		}
		r := Record{WorkspaceID: k.ws, SellerType: id.SellerType, FirstName: id.FirstName, MiddleName: id.MiddleName,
			LastName: id.LastName, LegalName: id.LegalName, Address: id.Address, Country: id.Country, TINs: id.TINs,
			DateOfBirth: id.DateOfBirth, CompanyRegistrationNumber: id.CompanyRegistrationNumber, VATNumber: id.VATNumber,
			AccountIdentifier: id.AccountIdentifier, AccountHolder: id.AccountHolder, DetailsComplete: id.Complete,
			Activity: k.activity, Quarters: *f}
		for _, q := range f {
			r.Total.add(q)
		}
		rep.Records = append(rep.Records, r)
	}
	sort.Slice(rep.Records, func(i, j int) bool {
		a, b := rep.Records[i], rep.Records[j]
		if a.Country != b.Country {
			return a.Country < b.Country
		}
		if a.WorkspaceID != b.WorkspaceID {
			return a.WorkspaceID < b.WorkspaceID
		}
		return a.Activity < b.Activity
	})
	return rep, nil
}

// CSVHeader is the first line of the CSV file.
var CSVHeader = func() []string {
	h := []string{"year", "funding", "currency", "workspace_id", "seller_type", "first_name", "middle_name", "last_name",
		"legal_name", "primary_address", "country_of_residence", "tins", "date_of_birth", "company_registration_number",
		"vat_number", "financial_account_identifier", "financial_account_holder", "details_complete", "activity"}
	for _, p := range []string{"q1", "q2", "q3", "q4", "total"} {
		h = append(h, p+"_consideration_usd_micros", p+"_activities", p+"_fees_usd_micros", p+"_taxes_withheld_usd_micros")
	}
	return h
}()

// text is a seller's own words as a CSV cell. The file is opened in a spreadsheet, which runs a cell starting with =,
// +, -, @, a tab or a carriage return as a formula: such a cell is written after a single quote, so it reads as text.
func text(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

// Render writes rep in format: the bytes of the file.
func Render(rep Report, format string) ([]byte, error) {
	var buf bytes.Buffer
	switch format {
	case FormatJSON:
		enc := json.NewEncoder(&buf)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return nil, fmt.Errorf("platformreport: render: %w", err)
		}
	case FormatCSV:
		w := csv.NewWriter(&buf)
		if err := w.Write(CSVHeader); err != nil {
			return nil, err
		}
		i64 := func(v int64) string { return strconv.FormatInt(v, 10) }
		for _, r := range rep.Records {
			tins := make([]string, len(r.TINs))
			for i, t := range r.TINs {
				tins[i] = t.Jurisdiction + ":" + t.Number
			}
			line := []string{strconv.Itoa(rep.Year), rep.Funding, rep.Currency, r.WorkspaceID, r.SellerType, text(r.FirstName),
				text(r.MiddleName), text(r.LastName), text(r.LegalName), text(r.Address), r.Country, strings.Join(tins, ";"),
				r.DateOfBirth, text(r.CompanyRegistrationNumber), text(r.VATNumber), r.AccountIdentifier, text(r.AccountHolder),
				strconv.FormatBool(r.DetailsComplete), r.Activity}
			for _, q := range append(r.Quarters[:], r.Total) {
				line = append(line, i64(q.ConsiderationUSDMicros), i64(q.Activities), i64(q.FeesUSDMicros), i64(q.TaxesWithheldUSDMicros))
			}
			if err := w.Write(line); err != nil {
				return nil, err
			}
		}
		w.Flush()
		if err := w.Error(); err != nil {
			return nil, fmt.Errorf("platformreport: render: %w", err)
		}
	default:
		return nil, invalid("format is %q or %q, not %q", FormatCSV, FormatJSON, format)
	}
	return buf.Bytes(), nil
}

// Record keeps that file, rep rendered in format, was written by operator: its platform_reports row and its entry in
// the operator audit trail, together. Neither holds anything from inside the file but its record count and sha256.
func (g *Generator) Record(ctx context.Context, rep Report, format string, file []byte, operator string) (Run, error) {
	operator = strings.TrimSpace(operator)
	if operator == "" {
		return Run{}, invalid("operator is required: who is running the export")
	}
	sum := sha256.Sum256(file)
	run := Run{ID: "prp_" + uuid.NewString(), Year: rep.Year, Funding: rep.Funding, Format: format,
		GeneratedAt: rep.GeneratedAt, Operator: operator, Rows: len(rep.Records), SHA256: hex.EncodeToString(sum[:])}
	err := pgx.BeginFunc(ctx, g.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO platform_reports (id, year, funding, format, generated_at, operator, rows, sha256)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, run.ID, run.Year, run.Funding, run.Format, run.GeneratedAt, run.Operator,
			run.Rows, run.SHA256); err != nil {
			return err
		}
		_, err := operatoraudit.RecordIn(ctx, tx, operatoraudit.Entry{Actor: operator, Action: AuditAction,
			Target: fmt.Sprintf("platform-report:%d:%s:%s", run.Year, run.Funding, run.Format),
			Detail: fmt.Sprintf("%s: %d records, sha256 %s", run.ID, run.Rows, run.SHA256)})
		return err
	})
	if err != nil {
		return Run{}, fmt.Errorf("platformreport: record the run: %w", err)
	}
	return run, nil
}

// Export generates year's report of funding's money, renders it in format and records the run.
func (g *Generator) Export(ctx context.Context, year int, funding, format, operator string) ([]byte, Run, error) {
	if strings.TrimSpace(operator) == "" {
		return nil, Run{}, invalid("operator is required: who is running the export")
	}
	if format != FormatCSV && format != FormatJSON {
		return nil, Run{}, invalid("format is %q or %q, not %q", FormatCSV, FormatJSON, format)
	}
	rep, err := g.Generate(ctx, year, funding)
	if err != nil {
		return nil, Run{}, err
	}
	file, err := Render(rep, format)
	if err != nil {
		return nil, Run{}, err
	}
	run, err := g.Record(ctx, rep, format, file, operator)
	if err != nil {
		return nil, Run{}, err
	}
	return file, run, nil
}

// Runs lists the files written, newest first: of year, or of every year when it is 0.
func (g *Generator) Runs(ctx context.Context, year int) ([]Run, error) {
	rows, err := g.pool.Query(ctx, `SELECT id, year, funding, format, generated_at, operator, rows, sha256 FROM platform_reports
		WHERE $1 = 0 OR year = $1 ORDER BY generated_at DESC, id LIMIT 500`, year)
	if err != nil {
		return nil, fmt.Errorf("platformreport: runs: %w", err)
	}
	runs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Run, error) {
		var r Run
		return r, row.Scan(&r.ID, &r.Year, &r.Funding, &r.Format, &r.GeneratedAt, &r.Operator, &r.Rows, &r.SHA256)
	})
	if err != nil {
		return nil, fmt.Errorf("platformreport: runs: %w", err)
	}
	return runs, nil
}
