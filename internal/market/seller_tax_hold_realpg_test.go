package market

import (
	"context"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/envelope"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/sellertax"
)

// B32.41 — a seller with earnings and incomplete tax details is asked at their first earning and twice more, the
// reminder interval apart; after the last request the payout run skips them while their earnings keep clearing, and
// completing the details lets the next run pay them. On the migrated schema, asserted on the payout row and its
// journal postings.
func TestPayOut_SkipsASellerWithheldForTaxDetailsUntilTheyAreComplete(t *testing.T) {
	pool := migratedDB(t)
	ctx := context.Background()
	s := NewStore(pool)
	key := make([]byte, envelope.KEKLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	ring, err := envelope.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	tax := sellertax.NewStore(pool, ring, partners.NewRegistry(nil))
	const seller, buyer = "ws-b3241-seller", "ws-b3241-buyer"
	if _, err := pool.Exec(ctx, `INSERT INTO market_sellers (workspace_id, stripe_account_id, details_submitted, payouts_enabled)
		VALUES ($1, 'acct_b3241', true, true)`, seller); err != nil {
		t.Fatal(err)
	}
	clear := func(useID, invoiceID string, ulxc int64, paid time.Time) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, price_ulxc,
			charge, used_at, ran_at, metered_at) VALUES ($1, 'lst_b3241', 1, $2, $3, $4, 'billed', $5, $5, $5)`, useID, seller, buyer, ulxc, paid.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		if n, err := s.ClearInvoice(ctx, buyer, invoiceID, paid.Add(-2*time.Hour), paid, paid, false); err != nil || n != 1 {
			t.Fatalf("clear %s = %d, %v", invoiceID, n, err)
		}
	}
	// $100 of uses, paid 30 days ago: the seller's 85%, $85, is past its holdback.
	clear("use_b3241_1", "in_b3241_1", 1_000_000_000, time.Now().Add(-30*24*time.Hour))

	remind := func() int {
		t.Helper()
		sent, err := tax.Remind(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return sent[seller]
	}
	if n := remind(); n != 1 {
		t.Fatalf("the first request at the first earning = %d, want 1", n)
	}
	if n := remind(); n != 0 {
		t.Fatalf("a request went out %d before the reminder interval had passed", n)
	}
	tax.SetReminderEvery(time.Nanosecond)
	if n := remind(); n != 2 {
		t.Fatalf("the first reminder = %d, want request 2", n)
	}
	if d, _ := tax.Get(ctx, seller); d.WithheldSince != nil {
		t.Fatalf("held after the first reminder: %+v", d)
	}
	if n := remind(); n != 3 {
		t.Fatalf("the second reminder = %d, want request 3", n)
	}
	held, err := tax.Get(ctx, seller)
	if err != nil || held.WithheldSince == nil || held.Hold == "" {
		t.Fatalf("after the second reminder %+v (%v), want the payouts held, with why", held, err)
	}
	if n := remind(); n != 0 {
		t.Fatalf("a fourth request went out (%d)", n)
	}

	conn := &connectLive{}
	if _, err := s.PayOut(ctx, conn, time.Now()); err != nil {
		t.Fatal(err)
	}
	var payouts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM market_payouts WHERE workspace_id = $1`, seller).Scan(&payouts); err != nil {
		t.Fatal(err)
	}
	if payouts != 0 || len(conn.transfers) != 0 {
		t.Fatalf("a withheld seller was paid: %d payouts, transfers %v", payouts, conn.transfers)
	}
	if _, err := s.TakeAsCredits(ctx, nil, seller, time.Now()); !errors.Is(err, ErrTaxHold) {
		t.Fatalf("a withheld seller took their earnings as credits: %v", err)
	}
	// Their earnings keep clearing: $50 more, paid 20 days ago, is journalled to their holdback and past it.
	clear("use_b3241_2", "in_b3241_2", 500_000_000, time.Now().Add(-20*24*time.Hour))
	var holdback int64
	if err := pool.QueryRow(ctx, `SELECT p.amount_usd_micros FROM market_journal_postings p JOIN market_journal_entries e ON e.id = p.entry_id
		WHERE e.kind = 'clear' AND e.ref = 'use_b3241_2' AND p.account = $1`, SellerHoldback(seller)).Scan(&holdback); err != nil || holdback != -42_500_000 {
		t.Fatalf("the cleared use credited the seller's holdback %d (%v), want -42,500,000 µUSD", holdback, err)
	}

	done, err := tax.Put(ctx, seller, sellertax.Input{SellerType: sellertax.Individual, FirstName: "Ada", LastName: "Lovelace",
		Address: "1 Analytical Way, London", Country: "GB", TINs: &[]sellertax.TIN{{Jurisdiction: "GB", Number: "1234567890"}},
		DateOfBirth: ptrTo("1985-04-12"), AccountIdentifier: ptrTo("GB33BUKB20201555555555"), AccountHolder: "Ada Lovelace"})
	if err != nil || !done.Complete || done.WithheldSince != nil {
		t.Fatalf("completing the details = %+v, %v; want complete and no longer held", done, err)
	}
	if _, err := s.PayOut(ctx, conn, time.Now()); err != nil {
		t.Fatal(err)
	}
	var id string
	var gross int64
	if err := pool.QueryRow(ctx, `SELECT id, gross_usd_micros FROM market_payouts WHERE workspace_id = $1 AND method = 'stripe'`, seller).Scan(&id, &gross); err != nil {
		t.Fatalf("the payout after completing: %v", err)
	}
	var available int64
	if err := pool.QueryRow(ctx, `SELECT p.amount_usd_micros FROM market_journal_postings p JOIN market_journal_entries e ON e.id = p.entry_id
		WHERE e.kind = 'payout' AND e.ref = $1 AND p.account = $2`, id, SellerAvailable(seller)).Scan(&available); err != nil {
		t.Fatal(err)
	}
	if gross != 127_500_000 || available != gross || len(conn.transfers) != 1 || conn.transfers[0] != id {
		t.Fatalf("paid %d µUSD, journalled %d out of available, transfers %v; want $127.50 — both clears — sent once as %s", gross, available, conn.transfers, id)
	}
}

func ptrTo(v string) *string { return &v }
