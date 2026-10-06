package market

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// B32.27 — a refund or chargeback reverses every row a sale wrote. The design's worked example, a $20.00 rental of C
// (parent B at 10%, grandparent A at 20%), is released and B takes its 1,360,000 as credits; the buyer's refund then
// posts one reversal entry mirroring the sale's — the clearing, the fee and all three payees — and one market_refunds
// row whose reversed share is the sum of the three rows. B's available reads −1,360,000 µUSD, owed, and B's next
// 1,360,000 µUSD of earnings are not paid out; every account the sale touched nets to zero. A takedown's refund inside
// the holdback reverses every payee's holdback, and nothing of it is ever released.
func TestReversal_ARefundReversesEveryRowASaleWrote(t *testing.T) {
	pool := migratedDB(t)
	ctx := context.Background()
	s := NewStore(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	paid := now.Add(-40 * 24 * time.Hour)
	const buyer = "ws-b3227-buyer"
	for _, ws := range []string{buyer, "ws-b3227-a", "ws-b3227-b", "ws-b3227-c", "ws-b3227-a2", "ws-b3227-b2", "ws-b3227-c2"} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	live := func(account string, amount int64) Posting { return Posting{account, amount, "USD", "live"} }

	t.Run("a refund after B was paid out", func(t *testing.T) {
		const a, b, c, useID = "ws-b3227-a", "ws-b3227-b", "ws-b3227-c", "use_b3227_worked"
		clearRoyaltySale(t, s, pool, "b3227_worked", buyer, 20_000_000, paid,
			familyEdge{"lst_b3227_c", c, "lst_b3227_b", b, 1000},
			familyEdge{"lst_b3227_b", b, "lst_b3227_a", a, 2000})
		released := paid.Add(Holdback)
		if n, err := s.ReleaseDue(ctx, released); err != nil || n != 1 {
			t.Fatalf("release = %d, %v; want the rental released", n, err)
		}
		if p, err := s.TakeAsCredits(ctx, noCredits{}, b, released); err != nil || p.GrossUSDMicros != 1_360_000 {
			t.Fatalf("B takes its royalty as credits: %+v, %v; want 1,360,000 paid out", p, err)
		}

		if n, ok, err := s.ReverseInvoice(ctx, "in_b3227_worked", "buyer_refund", "re_b3227", 20_000_000, false); err != nil || !ok || n != 1 {
			t.Fatalf("refund = %d, %v, %v; want the one use reversed", n, ok, err)
		}
		j, err := s.JournalFor(ctx, useID)
		if err != nil {
			t.Fatal(err)
		}
		want := []Posting{
			live(AccountStripeClearing, -20_000_000),
			live(AccountMarketFee, 3_000_000),
			live(SellerAvailable(c), 15_300_000),
			live(SellerAvailable(b), 1_360_000),
			live(SellerAvailable(a), 340_000),
		}
		if len(j) != 3 || j[2].Kind != JournalReversal || !reflect.DeepEqual(j[2].Postings, want) {
			t.Fatalf("the refunded rental's journal = %+v; want its clear, its release and one reversal entry %v", j, want)
		}
		var refunds int
		var gross, reversed int64
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(gross_usd_micros), 0), COALESCE(sum(reversed_share_usd_micros), 0)
			FROM market_refunds WHERE use_id = $1`, useID).Scan(&refunds, &gross, &reversed); err != nil {
			t.Fatal(err)
		}
		if refunds != 1 || gross != 20_000_000 || reversed != 17_000_000 {
			t.Fatalf("market_refunds holds %d rows, gross %d, reversed %d; want one row reversing 17,000,000 of 20,000,000", refunds, gross, reversed)
		}

		// Every account the sale touched nets to zero over its clear, release and reversal.
		var unbalanced []string
		rows, err := pool.Query(ctx, `SELECT p.account FROM market_journal_postings p JOIN market_journal_entries e ON e.id = p.entry_id
			WHERE e.ref = $1 GROUP BY p.account HAVING sum(p.amount_usd_micros) <> 0`, useID)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var account string
			if err := rows.Scan(&account); err != nil {
				t.Fatal(err)
			}
			unbalanced = append(unbalanced, account)
		}
		if rows.Close(); rows.Err() != nil || len(unbalanced) != 0 {
			t.Fatalf("after the refund %v do not net to zero (%v)", unbalanced, rows.Err())
		}

		// B was paid before the refund: its available is below zero, owed from its future earnings.
		r, err := s.journalCheck(ctx, b, now)
		if err != nil || !r.OK() || r.JournalAvailableUSDMicros != -1_360_000 {
			t.Fatalf("B's journal after the refund: %+v, %v; want available at -1,360,000 µUSD, reconciled", r, err)
		}
		if p, err := s.SellerPayouts(ctx, nil, b, now); err != nil || p.OwedUSDMicros != 1_360_000 || p.AvailableUSDMicros != 0 {
			t.Fatalf("B's payouts after the refund: owed %d, available %d (%v); want 1,360,000 owed", p.OwedUSDMicros, p.AvailableUSDMicros, err)
		}
		for _, ws := range []string{a, c} {
			reconciles(t, s, ws, now, "the refund")
			if got := journalBalance(t, s, SellerAvailable(ws)); got != 0 {
				t.Fatalf("%s's available reads %d after the refund; want 0", ws, got)
			}
		}

		// B's next 1,360,000 µUSD of earnings — its own $1.60 sale — repay what it owes: none of it is paid out.
		next := paid.Add(20 * 24 * time.Hour)
		billedUse(t, pool, "use_b3227_next", b, buyer, 16_000_000, next.Add(-time.Hour))
		if n, err := s.ClearInvoice(ctx, buyer, "in_b3227_next", next.Add(-2*time.Hour), next, next, true); err != nil || n != 1 {
			t.Fatalf("clear B's next sale = %d, %v", n, err)
		}
		if n, err := s.ReleaseDue(ctx, now); err != nil || n != 1 {
			t.Fatalf("release B's next sale = %d, %v", n, err)
		}
		if _, err := s.TakeAsCredits(ctx, noCredits{}, b, now); !errors.Is(err, ErrNothingAvailable) {
			t.Fatalf("B takes credits after earning back what it owed: %v; want ErrNothingAvailable", err)
		}
		if got := journalBalance(t, s, SellerAvailable(b)); got != 0 {
			t.Fatalf("B's available reads %d once its next 1,360,000 is earned; want 0", got)
		}
		reconciles(t, s, b, now, "B's next sale")
	})

	t.Run("a takedown's refund inside the holdback", func(t *testing.T) {
		const a, b, c, useID = "ws-b3227-a2", "ws-b3227-b2", "ws-b3227-c2", "use_b3227_held"
		held := now.Add(-time.Hour)
		clearRoyaltySale(t, s, pool, "b3227_held", buyer, 20_000_000, held,
			familyEdge{"lst_b3227_c2", c, "lst_b3227_b2", b, 1000},
			familyEdge{"lst_b3227_b2", b, "lst_b3227_a2", a, 2000})
		if _, err := pool.Exec(ctx, `UPDATE market_listings SET review_status = 'taken_down', review_reason = 'an IP claim', taken_down_at = $1
			WHERE id = 'lst_b3227_c2'`, now); err != nil {
			t.Fatal(err)
		}
		if n, err := s.refundUses(ctx, "lst_b3227_c2"); err != nil || n != 1 {
			t.Fatalf("takedown refund = %d, %v; want the one use reversed", n, err)
		}
		var reversed int64
		if err := pool.QueryRow(ctx, `SELECT reversed_share_usd_micros FROM market_refunds WHERE use_id = $1`, useID).Scan(&reversed); err != nil || reversed != 17_000_000 {
			t.Fatalf("the takedown's market_refunds row reverses %d (%v); want all three rows, 17,000,000", reversed, err)
		}
		j, err := s.JournalFor(ctx, useID)
		if err != nil {
			t.Fatal(err)
		}
		want := []Posting{
			live(AccountStripeClearing, -20_000_000),
			live(AccountMarketFee, 3_000_000),
			live(SellerHoldback(c), 15_300_000),
			live(SellerHoldback(b), 1_360_000),
			live(SellerHoldback(a), 340_000),
		}
		if len(j) != 2 || j[1].Kind != JournalReversal || !reflect.DeepEqual(j[1].Postings, want) {
			t.Fatalf("the taken-down rental's journal = %+v; want its clear and one reversal entry %v", j, want)
		}
		if n, err := s.ReleaseDue(ctx, held.Add(Holdback)); err != nil || n != 0 {
			t.Fatalf("release after the holdback = %d, %v; want nothing released", n, err)
		}
		for _, ws := range []string{a, b, c} {
			if journalBalance(t, s, SellerHoldback(ws)) != 0 || journalBalance(t, s, SellerAvailable(ws)) != 0 {
				t.Fatalf("%s holds %d and has %d available after the refund; want neither", ws,
					journalBalance(t, s, SellerHoldback(ws)), journalBalance(t, s, SellerAvailable(ws)))
			}
			reconciles(t, s, ws, held.Add(Holdback), "the takedown's refund")
		}
	})
}
