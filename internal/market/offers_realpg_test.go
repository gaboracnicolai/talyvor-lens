package market

import (
	"context"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

// B32.18 — migration 0201 gives a listing priced before it one per_use commercial offer at that price, and its next
// use bills exactly what it billed before, on the use's row and on the meter.
func TestOffers_AListingPricedBeforeTheMigrationBillsItsOldPrice(t *testing.T) {
	ctx := context.Background()
	pool := migratedDB(t)
	// A second database migrated up to 0200, given a priced listing, then migrated to the end.
	conn, err := pgx.Connect(ctx, pool.Config().ConnConfig.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	name := pool.Config().ConnConfig.Database + "_offers"
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = conn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") })
	cfg := pool.Config().Copy()
	cfg.ConnConfig.Database = name
	mc, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer mc.Close(ctx)
	before := fstest.MapFS{}
	names, _ := fs.Glob(migrations.FS, "*.sql")
	for _, n := range names {
		if n < "0201" {
			b, _ := fs.ReadFile(migrations.FS, n)
			before[n] = &fstest.MapFile{Data: b}
		}
	}
	if _, err := dbmigrate.Run(ctx, mc, before); err != nil {
		t.Fatal(err)
	}
	const seller, buyer, oldPrice = "ws-seller", "ws-buyer", int64(2_500_000)
	for _, q := range []string{
		`INSERT INTO workspaces (id, name, cache_prefix, synthetic, company) VALUES ('ws-seller', 'ws-seller', 'ws-seller', true, true),
			('ws-buyer', 'ws-buyer', 'ws-buyer', true, true)`,
		`INSERT INTO market_listings (id, workspace_id, kind, title, price_per_use_ulxc) VALUES ('lst_old', 'ws-seller', 'prompt', 'Old adder', 2500000),
			('lst_old_free', 'ws-seller', 'prompt', 'Old free adder', 0)`,
		`INSERT INTO market_listing_versions (listing_id, version, artifact, artifact_sha256) VALUES
			('lst_old', 1, '{"template":"What is {{a}} + {{b}}?","model":"claude-haiku-4-5"}', 'x'),
			('lst_old_free', 1, '{"template":"What is {{a}} + {{b}}?","model":"claude-haiku-4-5"}', 'x')`,
	} {
		if _, err := mc.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := dbmigrate.Run(ctx, mc, migrations.FS); err != nil {
		t.Fatalf("migrating a database with listings: %v", err)
	}
	old, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	s := NewStore(old)

	l, err := s.Get(ctx, buyer, "lst_old")
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Offers) != 1 || l.Offers[0].Kind != OfferPerUse || l.Offers[0].Licence != LicenceCommercial || l.Offers[0].PriceUSDMicros != 250_000 {
		t.Fatalf("the old listing's offers = %+v; want one per_use commercial offer at 250,000 µUSD", l.Offers)
	}
	if free, err := s.Get(ctx, buyer, "lst_old_free"); err != nil || len(free.Offers) != 0 {
		t.Fatalf("the old free listing's offers = %+v, %v; want none", free.Offers, err)
	}

	meter := &stripeMeter{tried: map[string]int{}}
	u, err := s.Use(ctx, UseDeps{Runner: answers("3"), Meter: meter}, buyer, "", "lst_old", UseRequest{Variables: map[string]string{"a": "1", "b": "2"}})
	if err != nil {
		t.Fatal(err)
	}
	var charge string
	var price int64
	if err := old.QueryRow(ctx, `SELECT charge, price_ulxc FROM market_uses WHERE id = $1`, u.ID).Scan(&charge, &price); err != nil {
		t.Fatal(err)
	}
	if charge != ChargeBilled || price != oldPrice || len(meter.accepted) != 1 {
		t.Fatalf("the use's row = %s at %d µLXC, metered %v; want billed at exactly the old %d µLXC, metered once", charge, price, meter.accepted, oldPrice)
	}
	if free, err := s.Use(ctx, UseDeps{Runner: answers("3"), Meter: meter}, buyer, "", "lst_old_free",
		UseRequest{Variables: map[string]string{"a": "1", "b": "2"}}); err != nil || free.Charge != ChargeFree {
		t.Fatalf("the old free listing's use = %+v, %v; want it free", free, err)
	}
}
