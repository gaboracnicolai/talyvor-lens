package market

import (
	"context"
	"strings"
	"testing"
	"time"
)

// B32.43 — to a seller who agreed to self-billing, each weekly payout is also a self-billed invoice from them to Talyvor,
// taxed by the tax partner with the seller as the supplier. With LENS_SELF_BILLING_VAT on, a GB VAT-registered seller's
// $100.00 week adds 20,000,000 µUSD of VAT to the payout, posted −VAT to their available balance and +VAT to
// tax:GB:input; a DE business seller's invoice carries the reverse-charge note and no VAT; an unregistered seller's has
// none. With it off, the invoice is at zero VAT, under review, and no VAT is posted. On the migrated schema, asserted on
// the payout rows, the invoices and the journal.
func TestPayOut_SelfBilledInvoicesCarryTheSellersVAT(t *testing.T) {
	pool := migratedDB(t)
	ctx := context.Background()
	const gb, de, plain, review, stale, buyer = "ws-b3243-gb", "ws-b3243-de", "ws-b3243-plain", "ws-b3243-review", "ws-b3243-stale", "ws-b3243-buyer"
	m := newTaxedMarket(t, pool, true, []string{gb, de, plain, review, stale, buyer})
	s := m.store
	talyvor := Supplier{LegalName: "TALYVOR LTD", Address: "71-75 Shelton Street, London WC2H 9JQ", VATNumber: "GB123456789"}
	s.SetSelfBilling(SelfBilling{Partners: s.tax.Partners, Customer: talyvor, VAT: true})

	// Each seller is connected, has agreed to self-billing, and gave their tax details: GB and DE as VAT-registered
	// companies, plain as an individual with no VAT number.
	seller := func(ws, sellerType, name, country, vat string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO market_sellers (workspace_id, stripe_account_id, country, details_submitted, payouts_enabled)
			VALUES ($1, 'acct_' || $1, $2, true, true)`, ws, country); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO seller_tax_profiles (workspace_id, seller_type, legal_name, first_name, last_name, address,
			country, vat_number, vat_valid, vat_checked_at, self_billing_agreed_version)
			VALUES ($1, $2, $3, $3, 'Seller', '1 High Street', $4, $5, $5 <> '', CASE WHEN $5 <> '' THEN now() END, 'draft-2026-10')`,
			ws, sellerType, name, country, vat); err != nil {
			t.Fatal(err)
		}
	}
	seller(gb, "entity", "Kestrel Prompts Ltd", "GB", "GB234567890")
	seller(de, "entity", "Adler Agenten GmbH", "DE", "DE123456789")
	seller(plain, "individual", "Ana", "GB", "")
	seller(review, "entity", "Heron Tools Ltd", "GB", "GB345678901")
	// stale's number was stored as valid, but is not when the invoice is issued: it is checked again then.
	seller(stale, "entity", "Stale Numbers Ltd", "GB", "GB999999999")

	// Each earns $100.00: a sale of 117,647,059 µUSD, of which the seller's 85% is 100,000,000 µUSD.
	clear := func(useID, ws string, paid time.Time) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, price_ulxc,
			charge, used_at, ran_at, metered_at) VALUES ($1, 'lst_b3243', 1, $2, $3, 1176470590, 'billed', $4, $4, $4)`,
			useID, ws, buyer, paid.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		if n, err := s.ClearInvoice(ctx, buyer, "in_"+useID, paid.Add(-2*time.Hour), paid, paid, false); err != nil || n != 1 {
			t.Fatalf("clear %s = %d, %v", useID, n, err)
		}
	}
	clear("use_b3243_gb", gb, time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC))
	clear("use_b3243_de", de, time.Date(2026, 8, 20, 13, 0, 0, 0, time.UTC))
	clear("use_b3243_plain", plain, time.Date(2026, 8, 20, 14, 0, 0, 0, time.UTC))
	clear("use_b3243_stale", stale, time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC))
	clear("use_b3243_review", review, time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)) // available only by week 2

	// Monday 7 September 2026 (2026-W37), with VAT on; the Monday after, with it off.
	week1 := time.Date(2026, 9, 7, 1, 0, 0, 0, time.UTC)
	conn := &connectLive{}
	if _, err := s.PayOut(ctx, conn, week1); err != nil {
		t.Fatal(err)
	}
	s.SetSelfBilling(SelfBilling{Partners: s.tax.Partners, Customer: talyvor, VAT: false})
	if _, err := s.PayOut(ctx, conn, week1.AddDate(0, 0, 7)); err != nil {
		t.Fatal(err)
	}

	type payout struct {
		id                               string
		gross, vat, accountFee, fee, net int64
	}
	payoutOf := func(ws string) payout {
		t.Helper()
		var p payout
		if err := pool.QueryRow(ctx, `SELECT id, gross_usd_micros, vat_usd_micros, account_fee_usd_micros, payout_fee_usd_micros, net_usd_micros
			FROM market_payouts WHERE workspace_id = $1 AND method = 'stripe'`, ws).Scan(&p.id, &p.gross, &p.vat, &p.accountFee, &p.fee, &p.net); err != nil {
			t.Fatalf("%s's payout: %v", ws, err)
		}
		return p
	}
	// postings is the journal's postings for a payout, by entry kind and account.
	postings := func(payoutID string) map[string]int64 {
		t.Helper()
		entries, err := s.JournalFor(ctx, payoutID)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int64{}
		for _, e := range entries {
			for _, p := range e.Postings {
				out[e.Kind+" "+p.Account] += p.AmountUSDMicros
			}
		}
		return out
	}

	// GB: $100.00 of earnings and $20.00 of VAT paid together — $120.00 less Stripe's $2 + $0.55 — and the VAT posted to
	// the input VAT Talyvor reclaims.
	p := payoutOf(gb)
	if want := (payout{p.id, 100_000_000, 20_000_000, 2_000_000, 550_000, 117_450_000}); p != want {
		t.Fatalf("GB seller's payout = %+v, want %+v", p, want)
	}
	if got, want := postings(p.id), map[string]int64{
		"self_bill " + SellerAvailable(gb): -20_000_000, "self_bill tax:GB:input": 20_000_000,
		"payout " + SellerAvailable(gb): 120_000_000, "payout stripe:clearing": -117_450_000, "payout stripe:connect_fees": -2_550_000,
	}; len(got) != len(want) || !sameAmounts(got, want) {
		t.Fatalf("GB payout's journal = %v, want %v", got, want)
	}
	bill, err := s.SelfBillOf(ctx, gb, p.id)
	if err != nil {
		t.Fatal(err)
	}
	if bill.Number != "TEST-SB-000001" || !bill.Preview || bill.Treatment != "standard" || bill.RateBps != 2000 || bill.Jurisdiction != "GB" ||
		bill.NetUSDMicros != 100_000_000 || bill.VATUSDMicros != 20_000_000 || bill.GrossUSDMicros != 120_000_000 ||
		bill.Supplier.Name != "Kestrel Prompts Ltd" || bill.Supplier.VATNumber != "GB234567890" ||
		bill.Customer.Name != "TALYVOR LTD" || bill.Customer.VATNumber != "GB123456789" || bill.AgreementVersion != "draft-2026-10" {
		t.Fatalf("GB seller's self-billed invoice = %+v", bill)
	}
	// Its statement carries the invoice, and the VAT is a line: the lines still sum to what the payout paid.
	st, err := s.SellerStatement(ctx, gb, "2026-W37")
	if err != nil {
		t.Fatal(err)
	}
	lines, sum := map[string]int64{}, int64(0)
	for _, l := range st.Lines {
		lines[l.Kind] = l.AmountUSDMicros
		sum += l.AmountUSDMicros
	}
	if st.SelfBilledInvoice == nil || st.SelfBilledInvoice.ID != bill.ID || lines[LineSupplyVAT] != 20_000_000 || lines[LineCarriedForward] != 0 ||
		sum != 117_450_000 || st.NetUSDMicros != 117_450_000 || st.Payout == nil || st.Payout.VATUSDMicros != 20_000_000 {
		t.Fatalf("GB seller's statement = %+v (lines sum to %d)", st, sum)
	}

	// DE: reverse charged — the note, no VAT, nothing posted beside the payout.
	p = payoutOf(de)
	if p.gross != 100_000_000 || p.vat != 0 {
		t.Fatalf("DE seller's payout = %+v, want $100.00 and no VAT", p)
	}
	if bill, err = s.SelfBillOf(ctx, de, p.id); err != nil {
		t.Fatal(err)
	}
	if bill.Treatment != "reverse_charge" || bill.VATUSDMicros != 0 || !strings.HasPrefix(bill.Note, "Reverse charge") || bill.Number != "TEST-SB-000001" {
		t.Fatalf("DE seller's self-billed invoice = %+v, want reverse charged with its note", bill)
	}
	if got := postings(p.id); len(got) != 3 || got["payout "+SellerAvailable(de)] != 100_000_000 {
		t.Fatalf("DE payout's journal = %v, want the payout alone", got)
	}

	// The unregistered seller: no VAT.
	p = payoutOf(plain)
	if bill, err = s.SelfBillOf(ctx, plain, p.id); err != nil {
		t.Fatal(err)
	}
	if p.vat != 0 || bill.Treatment != "not_registered" || bill.VATUSDMicros != 0 || bill.Supplier.Name != "Ana Seller" || bill.Supplier.VATNumber != "" {
		t.Fatalf("unregistered seller's payout %+v and invoice %+v, want no VAT", p, bill)
	}

	// A VAT number no longer valid when the invoice is issued: no VAT, and no number printed.
	p = payoutOf(stale)
	if bill, err = s.SelfBillOf(ctx, stale, p.id); err != nil {
		t.Fatal(err)
	}
	if p.vat != 0 || bill.Treatment != "not_registered" || bill.VATUSDMicros != 0 || bill.Supplier.VATNumber != "" {
		t.Fatalf("stale VAT number: payout %+v and invoice %+v, want no VAT", p, bill)
	}

	// With LENS_SELF_BILLING_VAT off, a GB VAT-registered seller's invoice is at zero VAT, under review, and nothing is
	// posted to tax:GB:input.
	p = payoutOf(review)
	if bill, err = s.SelfBillOf(ctx, review, p.id); err != nil {
		t.Fatal(err)
	}
	if p.vat != 0 || bill.VATEnabled || bill.Treatment != TaxUnderReview || bill.Note != VATUnderReview || bill.VATUSDMicros != 0 {
		t.Fatalf("under review: payout %+v and invoice %+v, want zero VAT, under review", p, bill)
	}
	if st, err = s.SellerStatement(ctx, review, "2026-W38"); err != nil {
		t.Fatal(err)
	}
	if l := st.Lines[len(st.Lines)-3]; l.Kind != LineSupplyVAT || l.Label != VATUnderReview || l.AmountUSDMicros != 0 {
		t.Fatalf("under review: statement lines = %+v, want the VAT line under review at 0", st.Lines)
	}
	if b, err := s.JournalBalance(ctx, "tax:GB:input", ""); err != nil || b != 20_000_000 {
		t.Fatalf("tax:GB:input = %d, %v; want the GB seller's 20,000,000 alone", b, err)
	}
}

func sameAmounts(a, b map[string]int64) bool {
	for k, v := range b {
		if a[k] != v {
			return false
		}
	}
	return true
}
