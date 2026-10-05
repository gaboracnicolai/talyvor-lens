package market

import (
	"context"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

// billedUse records one billed, metered use of ulxc µLXC by buyer of seller's listing.
func billedUse(t *testing.T, pool *pgxpool.Pool, id, seller, buyer string, ulxc int64, at time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, price_ulxc,
		charge, used_at, ran_at, metered_at) VALUES ($1, 'lst_b3216', 1, $2, $3, $4, 'billed', $5, $5, $5)`, id, seller, buyer, ulxc, at); err != nil {
		t.Fatal(err)
	}
}

func journalBalance(t *testing.T, pool *pgxpool.Pool, account string) int64 {
	t.Helper()
	var b int64
	if err := pool.QueryRow(context.Background(), `SELECT COALESCE(sum(balance_usd_micros), 0)::bigint FROM market_journal_balances
		WHERE account = $1`, account).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

// B32.16 — a cleared $1.00 use posts one entry that sums to zero: +1,000,000 stripe:clearing, −150,000
// revenue:market_fee, −850,000 to the seller's holdback, live as its invoice was. The buyer's chargeback posts
// its exact mirror, and every account the two touched is back to zero.
func TestJournal_AClearedUseAndItsRefundMirrorEachOther(t *testing.T) {
	pool := migratedDB(t)
	ctx := context.Background()
	s := NewStore(pool)
	const seller, buyer = "ws-b3216-seller", "ws-b3216-buyer"
	paid := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	billedUse(t, pool, "use_b3216", seller, buyer, 10_000_000, paid.Add(-time.Hour)) // 10 LXC: $1.00
	if n, err := s.ClearInvoice(ctx, buyer, "in_b3216", paid.Add(-2*time.Hour), paid, paid, true); err != nil || n != 1 {
		t.Fatalf("clear = %d, %v", n, err)
	}
	cleared := []Posting{
		{AccountStripeClearing, 1_000_000, "USD"},
		{AccountMarketFee, -150_000, "USD"},
		{SellerHoldback(seller), -850_000, "USD"},
	}
	j, err := s.JournalFor(ctx, "use_b3216")
	if err != nil {
		t.Fatal(err)
	}
	if len(j) != 1 || j[0].Kind != JournalClear || j[0].Funding != "live" || !j[0].CreatedAt.Equal(paid) || !reflect.DeepEqual(j[0].Postings, cleared) {
		t.Fatalf("the cleared use's journal = %+v; want one live clear entry %v at %v", j, cleared, paid)
	}
	if got := journalBalance(t, pool, SellerHoldback(seller)); got != -850_000 {
		t.Fatalf("the seller's holdback balance = %d; want -850000", got)
	}

	if n, _, err := s.ReverseInvoice(ctx, "in_b3216", "chargeback", "dp_b3216", 0, true); err != nil || n != 1 {
		t.Fatalf("reverse = %d, %v", n, err)
	}
	j, err = s.JournalFor(ctx, "use_b3216")
	if err != nil {
		t.Fatal(err)
	}
	mirror := []Posting{
		{AccountStripeClearing, -1_000_000, "USD"},
		{AccountMarketFee, 150_000, "USD"},
		{SellerHoldback(seller), 850_000, "USD"},
	}
	if len(j) != 2 || j[1].Kind != JournalReversal || j[1].Funding != "live" || !reflect.DeepEqual(j[1].Postings, mirror) {
		t.Fatalf("the refunded use's journal = %+v; want its clear and then the reversal %v", j, mirror)
	}
	for _, a := range []string{AccountStripeClearing, AccountMarketFee, SellerHoldback(seller)} {
		if got := journalBalance(t, pool, a); got != 0 {
			t.Fatalf("%s reads %d after the sale and its reversal; want 0", a, got)
		}
	}
}

// B32.16 — the database keeps the journal honest: an entry whose postings do not sum to zero is refused when its
// transaction commits, and a posting can be neither changed nor deleted.
func TestJournal_AnUnbalancedEntryAndAnyChangeAreRefused(t *testing.T) {
	pool := migratedDB(t)
	ctx := context.Background()
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		_, err := PostJournalTx(ctx, tx, JournalClear, "use_unbalanced", "test", time.Now(),
			Posting{Account: AccountStripeClearing, AmountUSDMicros: 1_000_000},
			Posting{Account: SellerHoldback("ws-x"), AmountUSDMicros: -850_000})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "does not balance") {
		t.Fatalf("committing an entry 150,000 µUSD out = %v; want it refused as unbalanced", err)
	}
	var entries int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM market_journal_entries`).Scan(&entries); err != nil || entries != 0 {
		t.Fatalf("%d entries after the refused commit (%v); want none", entries, err)
	}

	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		_, err := PostJournalTx(ctx, tx, JournalClear, "use_balanced", "test", time.Now(),
			Posting{Account: AccountStripeClearing, AmountUSDMicros: 1_000_000},
			Posting{Account: AccountMarketFee, AmountUSDMicros: -150_000},
			Posting{Account: SellerHoldback("ws-x"), AmountUSDMicros: -850_000})
		return err
	}); err != nil {
		t.Fatalf("a balanced entry was refused: %v", err)
	}
	for _, q := range []string{
		`UPDATE market_journal_postings SET amount_usd_micros = amount_usd_micros + 1`,
		`DELETE FROM market_journal_postings`,
	} {
		if _, err := pool.Exec(ctx, q); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("%s = %v; want it refused", q, err)
		}
	}
}

// B32.16 — migration 0199 journals the history: an earning cleared before it posts its clear entry, and a refund
// of one its reversal, so the journal's balances start out equal to what market_earnings says.
func TestJournal_TheMigrationJournalsEarningsAlreadyCleared(t *testing.T) {
	pool := migratedDB(t)
	ctx := context.Background()
	// A second database migrated up to 0198, then given history, then migrated to the end.
	conn, err := pgx.Connect(ctx, pool.Config().ConnConfig.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	name := "lens_b3216_backfill_" + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	cfg := pool.Config().Copy()
	cfg.ConnConfig.Database = name
	t.Cleanup(func() { _, _ = conn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") })
	mc, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer mc.Close(ctx)
	before := fstest.MapFS{}
	names, _ := fs.Glob(migrations.FS, "*.sql")
	for _, n := range names {
		if n < "0199" {
			b, _ := fs.ReadFile(migrations.FS, n)
			before[n] = &fstest.MapFile{Data: b}
		}
	}
	if _, err := dbmigrate.Run(ctx, mc, before); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-48 * time.Hour)
	if _, err := mc.Exec(ctx, `INSERT INTO market_earnings (use_id, seller_workspace_id, gross_usd_micros, share_usd_micros, fee_usd_micros, invoice_id,
		cleared_at, payable_at, livemode) VALUES ('use_old_kept', 'ws-old', 2000000, 1700000, 300000, 'in_old', $1, $1, false),
		('use_old_refunded', 'ws-old', 1000000, 850000, 150000, 'in_old', $1, $1, false)`, at); err != nil {
		t.Fatal(err)
	}
	if _, err := mc.Exec(ctx, `INSERT INTO market_refunds (use_id, listing_id, buyer_workspace_id, seller_workspace_id, price_ulxc, gross_usd_micros,
		reversed_share_usd_micros, reason, cause) VALUES ('use_old_refunded', 'lst_old', 'ws-buyer', 'ws-old', 10000000, 1000000, 850000, 'r', 'buyer_refund')`); err != nil {
		t.Fatal(err)
	}
	if _, err := dbmigrate.Run(ctx, mc, migrations.FS); err != nil {
		t.Fatalf("migrating a database with history: %v", err)
	}
	var holdback, clearing, entries int64
	if err := mc.QueryRow(ctx, `SELECT
		(SELECT sum(balance_usd_micros) FROM market_journal_balances WHERE account = 'seller:ws-old:holdback')::bigint,
		(SELECT sum(balance_usd_micros) FROM market_journal_balances WHERE account = 'stripe:clearing')::bigint,
		(SELECT count(*) FROM market_journal_entries)`).Scan(&holdback, &clearing, &entries); err != nil {
		t.Fatal(err)
	}
	if holdback != -1_700_000 || clearing != 2_000_000 || entries != 3 {
		t.Fatalf("after the backfill: holdback %d, clearing %d, %d entries; want -1700000, 2000000 and 3 (two clears, one reversal)",
			holdback, clearing, entries)
	}
}
