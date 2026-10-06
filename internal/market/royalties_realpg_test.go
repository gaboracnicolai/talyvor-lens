package market

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// familyEdge is one edge of a seeded family tree: version 1 of child builds on version 1 of parent at share.
type familyEdge struct {
	child, childOwner, parent, parentOwner string
	share                                  int
}

// payeeRow is one market_earnings row of a sale, as the test reads it back.
type payeeRow struct {
	Payee string
	Kind  string
	Depth int
	Share int64
}

// clearRoyaltySale seeds the family tree, then has buyer use version 1 of the first edge's child for gross µUSD on
// one live invoice and clears it. It answers the sale's clear entry and its market_earnings rows.
func clearRoyaltySale(t *testing.T, s *Store, pool *pgxpool.Pool, id, buyer string, gross int64, paid time.Time, edges ...familyEdge) ([]Posting, []payeeRow) {
	t.Helper()
	ctx := context.Background()
	for _, e := range edges {
		for _, l := range [][2]string{{e.child, e.childOwner}, {e.parent, e.parentOwner}} {
			if _, err := pool.Exec(ctx, `INSERT INTO market_listings (id, workspace_id, kind, title) VALUES ($1, $2, 'prompt', $1) ON CONFLICT (id) DO NOTHING`,
				l[0], l[1]); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := pool.Exec(ctx, `INSERT INTO market_lineage (id, child_listing_id, child_version, parent_listing_id, parent_version, share_bps, source)
			VALUES ($1, $2, 1, $3, 1, $4, 'declared')`, "lin_"+e.child+"_"+e.parent, e.child, e.parent, e.share); err != nil {
			t.Fatal(err)
		}
	}
	useID := "use_" + id
	if _, err := pool.Exec(ctx, `INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, price_ulxc,
		charge, use_kind, used_at, ran_at, metered_at) VALUES ($1, $2, 1, $3, $4, $5, 'billed', 'rent', $6, $6, $6)`,
		useID, edges[0].child, edges[0].childOwner, buyer, gross*ulxcPerUSDMicro, paid.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ClearInvoice(ctx, buyer, "in_"+id, paid.Add(-2*time.Hour), paid, paid, true); err != nil || n != 1 {
		t.Fatalf("clear %s = %d, %v", id, n, err)
	}
	j, err := s.JournalFor(ctx, useID)
	if err != nil {
		t.Fatal(err)
	}
	if len(j) != 1 || j[0].Kind != JournalClear {
		t.Fatalf("the sale %s's journal = %+v; want one clear entry", id, j)
	}
	rows, err := pool.Query(ctx, `SELECT seller_workspace_id, kind, depth, share_usd_micros FROM market_earnings WHERE use_id = $1
		ORDER BY depth, seller_workspace_id`, useID)
	if err != nil {
		t.Fatal(err)
	}
	earned, err := pgx.CollectRows(rows, pgx.RowToStructByPos[payeeRow])
	if err != nil {
		t.Fatal(err)
	}
	return j[0].Postings, earned
}

// B32.26 — each sale pays its originals, up the chain, capped. The design's worked example: a $20.00 rental of C,
// whose parent B is at 10% and B's parent A at 20%, posts one entry — +20,000,000 stripe:clearing, −3,000,000
// revenue:market_fee, −15,300,000 to C, −1,360,000 to B and −340,000 to A, all in holdback — and its release 14 days
// later moves every payee to available in one entry. With A linked to the buyer, B keeps 1,700,000; three parents at
// 30% each together receive 8,499,999 of a 17,000,000 pool and the seller 8,500,001; an eight-generation chain stops
// after five.
func TestRoyalties_EachSalePaysItsOriginalsUpTheChainCapped(t *testing.T) {
	pool := migratedDB(t)
	ctx := context.Background()
	s := NewStore(pool)
	paid := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	const buyer, linkedBuyer = "ws-b3226-buyer", "ws-b3226-buyer-2"
	for _, ws := range []string{buyer, linkedBuyer, "ws-b3226-a", "ws-b3226-a2", "ws-b3226-b", "ws-b3226-c"} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	live := func(account string, amount int64) Posting { return Posting{account, amount, "USD", "live"} }

	t.Run("the worked example", func(t *testing.T) {
		postings, earned := clearRoyaltySale(t, s, pool, "b3226_worked", buyer, 20_000_000, paid,
			familyEdge{"lst_b3226_c", "ws-b3226-c", "lst_b3226_b", "ws-b3226-b", 1000},
			familyEdge{"lst_b3226_b", "ws-b3226-b", "lst_b3226_a", "ws-b3226-a", 2000})
		want := []Posting{
			live(AccountStripeClearing, 20_000_000),
			live(AccountMarketFee, -3_000_000),
			live(SellerHoldback("ws-b3226-c"), -15_300_000),
			live(SellerHoldback("ws-b3226-b"), -1_360_000),
			live(SellerHoldback("ws-b3226-a"), -340_000),
		}
		if !reflect.DeepEqual(postings, want) {
			t.Fatalf("the rental's clear entry = %v; want %v", postings, want)
		}
		wantRows := []payeeRow{{"ws-b3226-c", EarningSale, 0, 15_300_000}, {"ws-b3226-b", EarningLineage, 1, 1_360_000},
			{"ws-b3226-a", EarningLineage, 2, 340_000}}
		if !reflect.DeepEqual(earned, wantRows) {
			t.Fatalf("the rental's earnings = %+v; want one row per payee %+v", earned, wantRows)
		}

		// The same 14-day holdback for every payee, released in one entry; A can then be paid what it earned.
		if n, err := s.ReleaseDue(ctx, paid.Add(Holdback)); err != nil || n != 1 {
			t.Fatalf("release = %d, %v; want the one use released", n, err)
		}
		j, err := s.JournalFor(ctx, "use_b3226_worked")
		if err != nil {
			t.Fatal(err)
		}
		if len(j) != 2 || j[1].Kind != JournalRelease || len(j[1].Postings) != 6 {
			t.Fatalf("after the holdback the journal = %+v; want the clear and one release entry of six postings", j)
		}
		for ws, amount := range map[string]int64{"ws-b3226-c": 15_300_000, "ws-b3226-b": 1_360_000, "ws-b3226-a": 340_000} {
			if got := journalBalance(t, s, SellerAvailable(ws)); got != -amount {
				t.Fatalf("%s's available reads %d µUSD; want %d", ws, got, -amount)
			}
			e, err := s.SellerEarnings(ctx, ws, paid.Add(Holdback))
			if err != nil || e.AvailableUSDMicros != amount {
				t.Fatalf("%s's earnings say %d µUSD available (%v); want %d", ws, e.AvailableUSDMicros, err, amount)
			}
			if r, err := s.journalCheck(ctx, ws, paid.Add(Holdback)); err != nil || !r.OK() {
				t.Fatalf("%s's journal does not reconcile: %+v, %v", ws, r, err)
			}
		}
	})

	t.Run("an ancestor linked to the buyer earns nothing", func(t *testing.T) {
		for _, ws := range []string{linkedBuyer, "ws-b3226-a2"} {
			if _, err := pool.Exec(ctx, `INSERT INTO workspace_card_fingerprints (workspace_id, fingerprint_hash) VALUES ($1, 'card-b3226')`, ws); err != nil {
				t.Fatal(err)
			}
		}
		postings, earned := clearRoyaltySale(t, s, pool, "b3226_linked", linkedBuyer, 20_000_000, paid,
			familyEdge{"lst_b3226_c2", "ws-b3226-c", "lst_b3226_b2", "ws-b3226-b", 1000},
			familyEdge{"lst_b3226_b2", "ws-b3226-b", "lst_b3226_a2", "ws-b3226-a2", 2000})
		want := []Posting{
			live(AccountStripeClearing, 20_000_000),
			live(AccountMarketFee, -3_000_000),
			live(SellerHoldback("ws-b3226-c"), -15_300_000),
			live(SellerHoldback("ws-b3226-b"), -1_700_000),
		}
		if !reflect.DeepEqual(postings, want) || len(earned) != 2 {
			t.Fatalf("with A linked to the buyer the entry = %v and the earnings %+v; want B to keep 1,700,000: %v", postings, earned, want)
		}
	})

	t.Run("three parents at 30% are capped at half the pool", func(t *testing.T) {
		var edges []familyEdge
		for i := 1; i <= 3; i++ {
			edges = append(edges, familyEdge{"lst_b3226_c3", "ws-b3226-c", fmt.Sprintf("lst_b3226_p%d", i), fmt.Sprintf("ws-b3226-p%d", i), 3000})
		}
		postings, earned := clearRoyaltySale(t, s, pool, "b3226_cap", buyer, 20_000_000, paid, edges...)
		want := []Posting{
			live(AccountStripeClearing, 20_000_000),
			live(AccountMarketFee, -3_000_000),
			live(SellerHoldback("ws-b3226-c"), -8_500_001),
			live(SellerHoldback("ws-b3226-p1"), -2_833_333),
			live(SellerHoldback("ws-b3226-p2"), -2_833_333),
			live(SellerHoldback("ws-b3226-p3"), -2_833_333),
		}
		if !reflect.DeepEqual(postings, want) || len(earned) != 4 {
			t.Fatalf("three parents at 30%%: the entry = %v and the earnings %+v; want them to receive 8,499,999 together: %v", postings, earned, want)
		}
	})

	t.Run("an eight-generation chain stops after five", func(t *testing.T) {
		var edges []familyEdge
		child, owner := "lst_b3226_g0", "ws-b3226-c"
		for g := 1; g <= 8; g++ {
			parent, parentOwner := fmt.Sprintf("lst_b3226_g%d", g), fmt.Sprintf("ws-b3226-g%d", g)
			edges = append(edges, familyEdge{child, owner, parent, parentOwner, 1000})
			child, owner = parent, parentOwner
		}
		_, earned := clearRoyaltySale(t, s, pool, "b3226_chain", buyer, 20_000_000, paid, edges...)
		want := []payeeRow{{"ws-b3226-c", EarningSale, 0, 15_300_000}, {"ws-b3226-g1", EarningLineage, 1, 1_530_000},
			{"ws-b3226-g2", EarningLineage, 2, 153_000}, {"ws-b3226-g3", EarningLineage, 3, 15_300},
			{"ws-b3226-g4", EarningLineage, 4, 1_530}, {"ws-b3226-g5", EarningLineage, 5, 170}}
		if !reflect.DeepEqual(earned, want) {
			t.Fatalf("an eight-generation chain's earnings = %+v; want five generations paid: %+v", earned, want)
		}
	})
}
