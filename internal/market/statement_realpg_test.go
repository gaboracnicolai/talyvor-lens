package market

import (
	"context"
	"testing"
	"time"
)

// B32.42 — sellers are paid weekly, each payout with a statement of its week. Two weeks of clearings make two payouts,
// Stripe's $2 account fee on the first of the month only; each statement's lines sum to its net and equal the seller's
// journal postings for its week; a seller under $25 is carried to the next week. On the migrated schema, asserted on
// the payout rows and the journal.
func TestPayOut_WeeklyWithAStatementOfEachWeek(t *testing.T) {
	pool := migratedDB(t)
	ctx := context.Background()
	s := NewStore(pool)
	const sellerA, sellerB, buyer = "ws-b3242-a", "ws-b3242-b", "ws-b3242-buyer"
	for _, ws := range []string{sellerA, sellerB} {
		if _, err := pool.Exec(ctx, `INSERT INTO market_sellers (workspace_id, stripe_account_id, details_submitted, payouts_enabled)
			VALUES ($1, 'acct_' || $1, true, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	// clear bills one use of seller's listing for ulxc (10 µLXC a µUSD) and clears it on a paid invoice; taxed uses carry
	// 20% GB VAT, which the buyer paid beside the price.
	clear := func(useID, seller string, ulxc int64, paid time.Time, taxed bool) {
		t.Helper()
		var taxMetered *time.Time
		if taxed {
			taxMetered = &paid
		}
		if _, err := pool.Exec(ctx, `INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, price_ulxc,
			charge, used_at, ran_at, metered_at, tax_metered_at) VALUES ($1, 'lst_b3242', 1, $2, $3, $4, 'billed', $5, $5, $5, $6)`,
			useID, seller, buyer, ulxc, paid.Add(-time.Hour), taxMetered); err != nil {
			t.Fatal(err)
		}
		if taxed {
			if _, err := pool.Exec(ctx, `INSERT INTO market_tax_lines (use_id, jurisdiction, rate_bps, taxable_usd_micros, tax_usd_micros, treatment,
				note, evidence, partner) VALUES ($1, 'GB', 2000, $2, $3, 'standard', '', '{}', 'test')`, useID, ulxc/ulxcPerUSDMicro, ulxc/ulxcPerUSDMicro/5); err != nil {
				t.Fatal(err)
			}
		}
		if n, err := s.ClearInvoice(ctx, buyer, "in_"+useID, paid.Add(-2*time.Hour), paid, paid, false); err != nil || n != 1 {
			t.Fatalf("clear %s = %d, %v", useID, n, err)
		}
	}
	// Monday 7 September 2026 (2026-W37) and the Monday after (2026-W38): both in September.
	week1 := time.Date(2026, 9, 7, 1, 0, 0, 0, time.UTC)
	week2 := week1.AddDate(0, 0, 7)
	// Past their 14-day holdback by week 1: A's $40 sale, with $8 VAT, and B's $20. By week 2: A's $50 and B's $20.
	clear("use_b3242_a1", sellerA, 400_000_000, time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC), true)
	clear("use_b3242_b1", sellerB, 200_000_000, time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC), false)
	clear("use_b3242_a2", sellerA, 500_000_000, time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC), false)
	clear("use_b3242_b2", sellerB, 200_000_000, time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC), false)

	conn := &connectLive{}
	for _, run := range []time.Time{week1, week2, week2.Add(time.Hour)} { // the third run, in week 2 again, pays nobody twice
		if _, err := s.PayOut(ctx, conn, run); err != nil {
			t.Fatal(err)
		}
	}
	type row struct {
		period                      string
		gross, accountFee, fee, net int64
	}
	payouts := func(ws string) []row {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT period, gross_usd_micros, account_fee_usd_micros, payout_fee_usd_micros, net_usd_micros
			FROM market_payouts WHERE workspace_id = $1 AND method = 'stripe' ORDER BY period`, ws)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.period, &r.gross, &r.accountFee, &r.fee, &r.net); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		return out
	}
	// A: $34.00 in week 1 less $2 + $0.33; $42.50 in week 2 less $0.36 only.
	if got, want := payouts(sellerA), []row{{"2026-W37", 34_000_000, 2_000_000, 330_000, 31_670_000}, {"2026-W38", 42_500_000, 0, 360_000, 42_140_000}}; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("seller A's payouts = %+v, want %+v", got, want)
	}
	// B: $17 in week 1 is under $25 and carried; week 2 pays $34.00 — B's first payout of the month, so with the $2.
	if got, want := payouts(sellerB), []row{{"2026-W38", 34_000_000, 2_000_000, 330_000, 31_670_000}}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("seller B's payouts = %+v, want %+v", got, want)
	}

	// Each statement's lines sum to its net, and equal the seller's journal postings for its week: what was released to
	// their available balance and what the payout took from it.
	journal := func(ws string, from, to time.Time) (before, released, paidOut int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT
			COALESCE(-sum(p.amount_usd_micros) FILTER (WHERE e.created_at < $2), 0)::bigint,
			COALESCE(-sum(p.amount_usd_micros) FILTER (WHERE e.created_at >= $2 AND e.kind = 'release'), 0)::bigint,
			COALESCE(sum(p.amount_usd_micros) FILTER (WHERE e.created_at >= $2 AND e.kind = 'payout'), 0)::bigint
			FROM market_journal_postings p JOIN market_journal_entries e ON e.id = p.entry_id
			WHERE p.account = $1 AND e.created_at < $3`, SellerAvailable(ws), from, to).Scan(&before, &released, &paidOut); err != nil {
			t.Fatal(err)
		}
		return before, released, paidOut
	}
	check := func(ws, period string, wantLines map[string]int64, wantNet, wantVAT int64) {
		t.Helper()
		st, err := s.SellerStatement(ctx, ws, period)
		if err != nil {
			t.Fatal(err)
		}
		lines, sum := map[string]int64{}, int64(0)
		for _, l := range st.Lines {
			lines[l.Kind] = l.AmountUSDMicros
			sum += l.AmountUSDMicros
		}
		for k, v := range wantLines {
			if lines[k] != v {
				t.Errorf("%s %s: line %s = %d, want %d (lines %+v)", ws, period, k, lines[k], v, st.Lines)
			}
		}
		if sum != wantNet || st.NetUSDMicros != wantNet {
			t.Errorf("%s %s: lines sum to %d, net %d; want %d", ws, period, sum, st.NetUSDMicros, wantNet)
		}
		if (st.Payout == nil) != (wantNet == 0) || (st.Payout != nil && st.Payout.NetUSDMicros != wantNet) {
			t.Errorf("%s %s: payout %+v, want one that paid %d", ws, period, st.Payout, wantNet)
		}
		if st.VATCollectedUSDMicros != wantVAT {
			t.Errorf("%s %s: VAT collected %d, want %d", ws, period, st.VATCollectedUSDMicros, wantVAT)
		}
		before, released, paidOut := journal(ws, st.From, st.To)
		if lines[LineBroughtForward] != before ||
			lines[LineSales]+lines[LineTalyvorFee]+lines[LineRoyaltiesPaid]+lines[LineRoyaltiesReceived] != released ||
			wantNet-lines[LineStripeFees] != paidOut ||
			lines[LineCarriedForward] != -(before+released-paidOut) {
			t.Errorf("%s %s: lines %+v do not equal the journal's week: brought forward %d, released %d, paid out %d",
				ws, period, st.Lines, before, released, paidOut)
		}
	}
	check(sellerA, "2026-W37", map[string]int64{LineSales: 40_000_000, LineTalyvorFee: -6_000_000, LineStripeFees: -2_330_000}, 31_670_000, 8_000_000)
	check(sellerA, "2026-W38", map[string]int64{LineSales: 50_000_000, LineTalyvorFee: -7_500_000, LineStripeFees: -360_000}, 42_140_000, 0)
	check(sellerB, "2026-W37", map[string]int64{LineSales: 20_000_000, LineTalyvorFee: -3_000_000, LineCarriedForward: -17_000_000}, 0, 0)
	check(sellerB, "2026-W38", map[string]int64{LineBroughtForward: 17_000_000, LineSales: 20_000_000, LineTalyvorFee: -3_000_000,
		LineCarriedForward: 0, LineStripeFees: -2_330_000}, 31_670_000, 0)

	if list, err := s.SellerStatements(ctx, sellerA); err != nil || len(list) != 2 || list[0].Period != "2026-W38" || list[1].Period != "2026-W37" {
		t.Fatalf("seller A's statements = %+v, %v; want W38 then W37", list, err)
	}
	if _, err := s.SellerStatement(ctx, sellerA, "2026-W54"); err == nil {
		t.Fatal("a week that does not exist was read")
	}
}
