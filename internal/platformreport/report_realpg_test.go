package platformreport

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/envelope"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/sellertax"
	"github.com/talyvor/lens/migrations"
)

// B32.44 — the annual platform-reporting export and the VAT return figures, on a migrated Postgres, asserted on the
// journal's postings.

func reportPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv("LENS_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	name := fmt.Sprintf("lens_platformreport_%d", time.Now().UnixNano())
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
		if c, err := pgx.Connect(context.Background(), admin); err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			_ = c.Close(context.Background())
		}
	})
	return pool
}

func ptr(v string) *string { return &v }

// A seeded 2026: a UK individual, a German company and a US individual sell, and a seller who never gave a country.
// The export lists the first two, with each quarter's consideration and fees as the journal posted them and their
// TINs in clear only inside the file; leaves out the US seller; names the seller with no country as unresolved; and
// records the file's sha256 and its run. The GB return for 2026Q4 equals that quarter's GB tax lines.
func TestPlatformReport_ListsUKAndEUSellersFromTheJournalAndTheGBReturnEqualsTheQuartersTaxLines(t *testing.T) {
	pool := reportPool(t)
	ctx := context.Background()
	key := make([]byte, envelope.KEKLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	ring, err := envelope.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	sellers := sellertax.NewStore(pool, ring, partners.NewRegistry(nil))
	m := market.NewStore(pool)
	const uk, de, us, nowhere, buyer = "ws-b3244-uk", "ws-b3244-de", "ws-b3244-us", "ws-b3244-nowhere", "ws-b3244-buyer"
	const ukTIN, deTIN, ukAccount = "UTR1234567890", "DE12345678901", "GB33BUKB20201555555555"

	for ws, in := range map[string]sellertax.Input{
		uk: {SellerType: sellertax.Individual, FirstName: "Ada", LastName: "Lovelace", Address: "1 Analytical Way, London",
			Country: "GB", TINs: &[]sellertax.TIN{{Jurisdiction: "GB", Number: ukTIN}}, DateOfBirth: ptr("1985-04-12"),
			AccountIdentifier: ptr(ukAccount), AccountHolder: "Ada Lovelace"},
		de: {SellerType: sellertax.Entity, LegalName: "Rechenwerk GmbH", CompanyRegistrationNumber: "HRB12345",
			Address: "Hauptstr. 1, Berlin", Country: "DE", TINs: &[]sellertax.TIN{{Jurisdiction: "DE", Number: deTIN}},
			AccountIdentifier: ptr("DE89370400440532013000"), AccountHolder: "Rechenwerk GmbH"},
		us: {SellerType: sellertax.Individual, FirstName: "Grace", LastName: "Hopper", Address: "1 Navy Way, Arlington",
			Country: "US", TINs: &[]sellertax.TIN{{Jurisdiction: "US", Number: "123456789"}}, DateOfBirth: ptr("1980-12-09"),
			AccountIdentifier: ptr("000123456789"), AccountHolder: "Grace Hopper"},
	} {
		if _, err := sellers.Put(ctx, ws, in); err != nil {
			t.Fatalf("save %s: %v", ws, err)
		}
	}

	// sell clears one billed use of seller's at usd micros, paid at paid, with its buyer's tax line.
	n := 0
	sell := func(seller string, usd int64, paid time.Time, jurisdiction string, rateBps int) {
		t.Helper()
		n++
		use, invoice := fmt.Sprintf("use_b3244_%d", n), fmt.Sprintf("in_b3244_%d", n)
		used := paid.Add(-time.Hour)
		if _, err := pool.Exec(ctx, `INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, price_ulxc,
			charge, used_at, ran_at, metered_at, tax_metered_at) VALUES ($1, 'lst_b3244', 1, $2, $3, $4, 'billed', $5, $5, $5, $5)`,
			use, seller, buyer, usd*10, used); err != nil {
			t.Fatal(err)
		}
		treatment := "standard"
		if rateBps == 0 {
			treatment = "reverse_charge"
		}
		if _, err := pool.Exec(ctx, `INSERT INTO market_tax_lines (use_id, jurisdiction, rate_bps, taxable_usd_micros, tax_usd_micros,
			treatment, note, evidence, partner) VALUES ($1, $2, $3, $4, $5, $6, '', '{}', 'test')`,
			use, jurisdiction, rateBps, usd, usd*int64(rateBps)/10000, treatment); err != nil {
			t.Fatal(err)
		}
		if got, err := m.ClearInvoice(ctx, buyer, invoice, used.Add(-time.Minute), paid, paid, false); err != nil || got != 1 {
			t.Fatalf("clear %s = %d, %v", use, got, err)
		}
	}
	day := func(y int, mo time.Month, d int) time.Time { return time.Date(y, mo, d, 12, 0, 0, 0, time.UTC) }
	sell(uk, 10_000_000, day(2025, 12, 20), "GB", 2000) // the year before: not in 2026's report or its Q4 return
	sell(uk, 10_000_000, day(2026, 2, 10), "GB", 2000)
	sell(uk, 20_000_000, day(2026, 10, 2), "GB", 2000)
	sell(uk, 4_000_000, day(2026, 10, 3), "DE", 0) // a DE business buyer: reverse charged, not in the GB return
	sell(de, 50_000_000, day(2026, 5, 12), "GB", 2000)
	sell(de, 30_000_000, day(2026, 10, 4), "GB", 2000)
	sell(us, 40_000_000, day(2026, 10, 4), "GB", 2000)
	sell(nowhere, 5_000_000, day(2026, 7, 1), "GB", 2000)

	g := New(pool, sellers)
	file, run, err := g.Export(ctx, 2026, FundingTest, FormatCSV, "nicolai")
	if err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(bytes.NewReader(file)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	col := map[string]int{}
	for i, h := range recs[0] {
		col[h] = i
	}
	byWS := map[string][]string{}
	for _, r := range recs[1:] {
		byWS[r[col["workspace_id"]]] = r
	}
	if len(recs) != 3 || byWS[uk] == nil || byWS[de] == nil {
		t.Fatalf("the export's records = %v, want the UK and the German seller only", recs[1:])
	}

	// Each quarter's consideration and fees are what the journal posted that quarter: the seller's holdback credits
	// and the market fee of their own sales.
	journal := func(ws, account string, q int) int64 {
		t.Helper()
		from := time.Date(2026, time.Month(3*(q-1)+1), 1, 0, 0, 0, 0, time.UTC)
		var sum int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(-sum(p.amount_usd_micros), 0)::bigint FROM market_journal_postings p
			JOIN market_journal_entries j ON j.id = p.entry_id JOIN market_uses u ON u.id = j.ref
			WHERE j.kind IN ('clear', 'reversal') AND u.seller_workspace_id = $1 AND p.account = $2
			  AND j.created_at >= $3 AND j.created_at < $4`, ws, account, from, from.AddDate(0, 3, 0)).Scan(&sum); err != nil {
			t.Fatal(err)
		}
		return sum
	}
	want := map[string][4][3]int64{ // consideration, activities, fees per quarter
		uk: {{8_500_000, 1, 1_500_000}, {}, {}, {20_400_000, 2, 3_600_000}},
		de: {{}, {42_500_000, 1, 7_500_000}, {}, {25_500_000, 1, 4_500_000}},
	}
	for ws, quarters := range want {
		r := byWS[ws]
		for q := 1; q <= 4; q++ {
			p := fmt.Sprintf("q%d_", q)
			got := [3]string{r[col[p+"consideration_usd_micros"]], r[col[p+"activities"]], r[col[p+"fees_usd_micros"]]}
			w := quarters[q-1]
			if got != [3]string{fmt.Sprint(w[0]), fmt.Sprint(w[1]), fmt.Sprint(w[2])} {
				t.Errorf("%s Q%d = consideration, activities, fees %v; want %v", ws, q, got, w)
			}
			if c, f := journal(ws, market.SellerHoldback(ws), q), journal(ws, market.AccountMarketFee, q); fmt.Sprint(c) != got[0] || fmt.Sprint(f) != got[2] {
				t.Errorf("%s Q%d: the journal posted consideration %d and fees %d, the file says %s and %s", ws, q, c, f, got[0], got[2])
			}
			if r[col[p+"taxes_withheld_usd_micros"]] != "0" {
				t.Errorf("%s Q%d withheld %s, want 0", ws, q, r[col[p+"taxes_withheld_usd_micros"]])
			}
		}
	}
	if r := byWS[uk]; r[col["tins"]] != "GB:"+ukTIN || r[col["date_of_birth"]] != "1985-04-12" ||
		r[col["financial_account_identifier"]] != ukAccount || r[col["activity"]] != ActivityDigitalListing {
		t.Errorf("the UK seller's identification = TINs %q, born %q, account %q, activity %q; want them in clear",
			r[col["tins"]], r[col["date_of_birth"]], r[col["financial_account_identifier"]], r[col["activity"]])
	}
	if r := byWS[de]; r[col["tins"]] != "DE:"+deTIN || r[col["legal_name"]] != "Rechenwerk GmbH" || r[col["company_registration_number"]] != "HRB12345" {
		t.Errorf("the German seller's identification = %v", r)
	}

	// The TIN is in clear inside the file only: not in the stored details, the run's row or the audit trail.
	var stored, recorded, audited string
	if err := pool.QueryRow(ctx, `SELECT t::text FROM seller_tax_profiles t WHERE workspace_id = $1`, uk).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT r::text FROM platform_reports r WHERE id = $1`, run.ID).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT a::text FROM operator_audit a WHERE action = $1 AND actor = 'nicolai'`, AuditAction).Scan(&audited); err != nil {
		t.Fatalf("the run's audit entry: %v", err)
	}
	for where, text := range map[string]string{"seller_tax_profiles": stored, "platform_reports": recorded, "operator_audit": audited} {
		if strings.Contains(text, ukTIN) || strings.Contains(text, deTIN) {
			t.Errorf("a TIN is in clear in %s: %s", where, text)
		}
	}

	// The run is recorded with the file's sha256.
	sum := sha256.Sum256(file)
	var rows int
	var sha string
	if err := pool.QueryRow(ctx, `SELECT rows, sha256 FROM platform_reports WHERE id = $1 AND year = 2026 AND funding = 'test'
		AND format = 'csv' AND operator = 'nicolai'`, run.ID).Scan(&rows, &sha); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || sha != hex.EncodeToString(sum[:]) || !strings.Contains(audited, sha) {
		t.Errorf("recorded %d rows, sha256 %s (audit %s); want 2 and the file's %x", rows, sha, audited, sum)
	}

	rep, err := g.Generate(ctx, 2026, FundingTest)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unresolved) != 1 || rep.Unresolved[0] != nowhere {
		t.Errorf("unresolved sellers = %v, want %s, who gave no country", rep.Unresolved, nowhere)
	}
	if live, err := g.Generate(ctx, 2026, FundingLive); err != nil || len(live.Records) != 0 {
		t.Errorf("the live report = %d records (%v); test money is never in it", len(live.Records), err)
	}

	// The GB return for 2026Q4 is the GB tax lines of the uses cleared in it: $20, $30 and $40 at 20%.
	ret, err := g.TaxReturn(ctx, "GB", "2026Q4", FundingTest)
	if err != nil {
		t.Fatal(err)
	}
	var lines, taxable, tax int64
	if err := pool.QueryRow(ctx, `SELECT count(*), sum(t.taxable_usd_micros), sum(t.tax_usd_micros) FROM market_tax_lines t
		JOIN market_uses u ON u.id = t.use_id WHERE t.jurisdiction = 'GB' AND u.cleared_at >= '2026-10-01' AND u.cleared_at < '2027-01-01'`).
		Scan(&lines, &taxable, &tax); err != nil {
		t.Fatal(err)
	}
	if lines != 3 || taxable != 90_000_000 || tax != 18_000_000 {
		t.Fatalf("seeded Q4 GB tax lines = %d, %d, %d", lines, taxable, tax)
	}
	if len(ret.Lines) != 1 || ret.Lines[0].Sales != lines || ret.TaxableUSDMicros != taxable || ret.TaxUSDMicros != tax ||
		ret.JournalTaxUSDMicros != tax || ret.Lines[0].Treatment != "standard" || ret.Lines[0].RateBps != 2000 {
		t.Errorf("the GB 2026Q4 return = %+v; want %d sales, %d taxable and %d tax, as the tax lines and the journal say", ret, lines, taxable, tax)
	}
}
