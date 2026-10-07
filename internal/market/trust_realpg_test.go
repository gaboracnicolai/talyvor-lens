package market

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// B32.49 — the trust panel: the review count and average use only buyers who paid and are not linked to the seller, a
// publisher with a claim upheld this year reads not verified, and the originals and remixes are the lineage read's.
func TestTrust_ReviewsVerifiedPublisherAndLineage(t *testing.T) {
	pool := migratedDB(t)
	ctx := context.Background()
	s := NewStore(pool)
	const seller, paidA, paidB, linkedC, unpaidD = "ws-b3249-seller", "ws-b3249-a", "ws-b3249-b", "ws-b3249-c", "ws-b3249-d"
	for _, ws := range []string{seller, paidA, paidB, linkedC, unpaidD} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	listing := func(id string) {
		t.Helper()
		exec(`INSERT INTO market_listings (id, workspace_id, kind, title) VALUES ($1, $2, 'prompt', $1)`, id, seller)
		exec(`INSERT INTO market_listing_versions (listing_id, version, artifact, artifact_sha256) VALUES ($1, 1, '{"template": "x"}', 'sha')`, id)
	}
	listing("lst_b3249")
	// A and B paid for a use, C paid while it was not yet linked to the seller, D never paid.
	for _, buyer := range []string{paidA, paidB, linkedC} {
		exec(`INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, price_ulxc, charge, used_at, ran_at)
			VALUES ($1, 'lst_b3249', 1, $2, $3, 1000, 'billed', now(), now())`, "use_b3249_"+buyer, seller, buyer)
	}
	exec(`INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, price_ulxc, charge, used_at, ran_at)
		VALUES ('use_b3249_free', 'lst_b3249', 1, $1, $2, 0, 'free', now(), now())`, seller, unpaidD)
	review := func(buyer string, rating int) error {
		_, err := s.Review(ctx, buyer, "lst_b3249", ReviewInput{Rating: rating, Text: "rated " + buyer})
		return err
	}

	t.Run("the review count and average use only paying, unlinked buyers", func(t *testing.T) {
		for buyer, rating := range map[string]int{paidA: 5, paidB: 4, linkedC: 1} {
			if err := review(buyer, rating); err != nil {
				t.Fatalf("%s's review: %v", buyer, err)
			}
		}
		if err := review(unpaidD, 1); !errors.Is(err, ErrNotReviewer) {
			t.Fatalf("a buyer who never paid reviewing = %v; want ErrNotReviewer", err)
		}
		if err := review(seller, 5); !errors.Is(err, ErrNotReviewer) {
			t.Fatalf("the seller reviewing its own listing = %v; want ErrNotReviewer", err)
		}
		// C is found to share a card with the seller after it reviewed: from then on it is one party with it.
		exec(`INSERT INTO workspace_card_fingerprints (workspace_id, fingerprint_hash) VALUES ($1, 'card-b3249'), ($2, 'card-b3249')`, seller, linkedC)
		if err := review(linkedC, 1); !errors.Is(err, ErrNotReviewer) {
			t.Fatalf("a linked buyer rewriting its review = %v; want ErrNotReviewer", err)
		}
		tr, err := s.Trust(ctx, unpaidD, "lst_b3249")
		if err != nil {
			t.Fatal(err)
		}
		if r := tr.Reviews; r.Count != 2 || r.Average != 4.5 || r.Stars != [5]int{0, 0, 0, 1, 1} || len(r.Recent) != 2 {
			t.Fatalf("the reviews = %+v; want A's 5 and B's 4 alone: 2, average 4.5", r)
		}
		// The seller replies to a review, and the panel shows it.
		replied, err := s.ReplyToReview(ctx, seller, "lst_b3249", tr.Reviews.Recent[0].ID, "Thank you!")
		if err != nil || replied.Reply != "Thank you!" || replied.RepliedAt == nil {
			t.Fatalf("the seller's reply = %+v, %v", replied, err)
		}
		if _, err := s.ReplyToReview(ctx, paidA, "lst_b3249", tr.Reviews.Recent[0].ID, "not mine"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a buyer replying to a review = %v; want ErrNotFound", err)
		}
	})

	t.Run("a publisher with a claim upheld this year reads not verified", func(t *testing.T) {
		exec(`INSERT INTO market_sellers (workspace_id, stripe_account_id, payouts_enabled) VALUES ($1, 'acct_b3249', true)`, seller)
		listing("lst_b3249_old")
		upheld := func(id, listingID string, decided time.Time) {
			exec(`INSERT INTO market_ip_claims (id, listing_id, claimant_workspace_id, original_reference, evidence, good_faith, status,
				filed_at, counter_by, decided_by, decided_at) VALUES ($1, $2, $3, 'https://example.com/x', 'copied', 'in good faith', 'upheld',
				$4, $4, 'operator', $4)`, id, listingID, paidA, decided)
		}
		upheld("mic_b3249_old", "lst_b3249_old", time.Now().AddDate(-1, -1, 0))
		tr, err := s.Trust(ctx, paidA, "lst_b3249")
		if err != nil {
			t.Fatal(err)
		}
		if p := tr.Publisher; !p.Verified || p.UpheldClaims12Months != 0 {
			t.Fatalf("payouts enabled, its one upheld claim 13 months old: publisher = %+v; want verified", p)
		}
		listing("lst_b3249_recent")
		upheld("mic_b3249_recent", "lst_b3249_recent", time.Now().AddDate(0, -2, 0))
		if tr, err = s.Trust(ctx, paidA, "lst_b3249"); err != nil {
			t.Fatal(err)
		}
		if p := tr.Publisher; p.Verified || p.UpheldClaims12Months != 1 || !p.PayoutsEnabled {
			t.Fatalf("a claim upheld two months ago: publisher = %+v; want not verified", p)
		}
		if tr.Claims != (ClaimCounts{}) {
			t.Fatalf("the claims against the listing itself = %+v; want none (the upheld ones are against others)", tr.Claims)
		}
	})

	t.Run("the originals and remixes equal the lineage read", func(t *testing.T) {
		listing("lst_b3249_original")
		listing("lst_b3249_remix")
		exec(`INSERT INTO market_lineage (id, child_listing_id, child_version, parent_listing_id, parent_version, share_bps, source)
			VALUES ('lin_b3249_up', 'lst_b3249', 1, 'lst_b3249_original', 1, 1500, 'declared'),
			       ('lin_b3249_down', 'lst_b3249_remix', 1, 'lst_b3249', 1, 0, 'declared')`)
		tr, err := s.Trust(ctx, paidB, "lst_b3249")
		if err != nil {
			t.Fatal(err)
		}
		lin, err := s.Lineage(ctx, paidB, "lst_b3249", 0)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(tr.Originals, lin.Ancestors) || tr.Remixes != lin.Descendants || len(tr.Originals) != 1 || tr.Remixes != 1 {
			t.Fatalf("the trust panel's originals %+v and remixes %d; the lineage read's %+v and %d", tr.Originals, tr.Remixes, lin.Ancestors, lin.Descendants)
		}
	})
}
