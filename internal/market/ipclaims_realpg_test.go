package market

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// B32.47 — an IP claim holds the listing's new earnings in escrow until the operator decides it: rejected releases
// them to available, attributed writes a claim edge so the next sale pays the claimant, and upheld takes the listing
// down and refunds them — each read on the journal, and each step in the operator audit trail.
func TestIPClaims_AClaimHoldsNewEarningsUntilItIsDecided(t *testing.T) {
	pool := migratedDB(t)
	ctx := context.Background()
	s := NewStore(pool)
	const seller, claimant, buyer = "ws-b3247-seller", "ws-b3247-claimant", "ws-b3247-buyer"
	for _, ws := range []string{seller, claimant, buyer} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	listing := func(id, owner string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO market_listings (id, workspace_id, kind, title) VALUES ($1, $2, 'prompt', $1)`, id, owner); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO market_listing_versions (listing_id, version, artifact, artifact_sha256)
			VALUES ($1, 1, '{"template": "x"}', 'sha')`, id); err != nil {
			t.Fatal(err)
		}
	}
	// sale records and clears a $20.00 use of listing by buyer at, paid an hour later on a live invoice.
	sale := func(id, listing string, at time.Time) []JournalEntry {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, price_ulxc,
			charge, used_at, ran_at, metered_at) VALUES ($1, $2, 1, $3, $4, $5, 'billed', $6, $6, $6)`,
			id, listing, seller, buyer, 20_000_000*ulxcPerUSDMicro, at); err != nil {
			t.Fatal(err)
		}
		if n, err := s.ClearInvoice(ctx, buyer, "in_"+id, at.Add(-time.Minute), at.Add(time.Minute), at.Add(time.Hour), true); err != nil || n != 1 {
			t.Fatalf("clear %s = %d, %v", id, n, err)
		}
		return journal(t, s, id)
	}
	audited := func(claimID string, want ...string) {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT action FROM operator_audit WHERE target = $1 ORDER BY id`, claimID)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var a string
			if err := rows.Scan(&a); err != nil {
				t.Fatal(err)
			}
			got = append(got, a)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("the audit trail of %s = %v; want %v", claimID, got, want)
		}
	}
	live := func(account string, amount int64) Posting { return Posting{account, amount, "USD", "live"} }
	filed := time.Now().Add(-60 * 24 * time.Hour).UTC().Truncate(time.Microsecond)

	t.Run("rejected releases the held earnings to available", func(t *testing.T) {
		listing("lst_b3247_a", seller)
		sale("use_b3247_before", "lst_b3247_a", filed.Add(-time.Hour))
		c, err := s.FileIPClaim(ctx, claimant, IPClaimFiling{ListingID: "lst_b3247_a", OriginalReference: "https://example.com/my-prompt",
			Evidence: "the same template, word for word", GoodFaith: "I believe in good faith that this copies my work."}, filed)
		if err != nil {
			t.Fatal(err)
		}
		if c.Status != ClaimOpen || !c.CounterBy.Equal(filed.Add(10*24*time.Hour)) || c.SellerWorkspaceID != seller {
			t.Fatalf("the filed claim = %+v; want open, counterable for 10 days, against %s", c, seller)
		}
		held := sale("use_b3247_held", "lst_b3247_a", filed.Add(time.Hour))
		paid := filed.Add(2 * time.Hour)
		// 20 days after it cleared, the release job moves the earning from before the claim, and not the held one.
		if _, err := s.ReleaseDue(ctx, paid.Add(20*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if j := journal(t, s, "use_b3247_before"); len(j) != 2 || j[1].Kind != JournalRelease {
			t.Fatalf("the use before the claim's journal = %+v; want its clear and its release", j)
		}
		if j := journal(t, s, "use_b3247_held"); !reflect.DeepEqual(j, held) {
			t.Fatalf("20 days after it cleared, the held use's journal = %+v; want its clear entry alone", j)
		}
		share := -held[0].Postings[2].AmountUSDMicros
		if held[0].Postings[2] != live(SellerHoldback(seller), -17_000_000) {
			t.Fatalf("the held sale's clear entry = %+v; want 17,000,000 µUSD to %s's holdback", held[0].Postings, seller)
		}
		if got := journalBalance(t, s, SellerHoldback(seller)); got != -share {
			t.Fatalf("%s's holdback = %d; want the held earning, %d", seller, got, -share)
		}
		if _, err := s.CounterIPClaim(ctx, seller, c.ID, "I wrote it myself; here are my drafts.", filed.Add(24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		out, err := s.DecideIPClaim(ctx, nil, c.ID, "ops@talyvor", IPClaimDecision{Outcome: ClaimRejected, Reason: "independent work"},
			paid.Add(20*24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if out.HoldsReleased != 1 || out.Claim.Status != ClaimRejected || out.Claim.HeldUses != 0 {
			t.Fatalf("the rejection = %+v; want one hold released and the claim rejected", out)
		}
		j := journal(t, s, "use_b3247_held")
		if len(j) != 2 || j[1].Kind != JournalRelease ||
			!reflect.DeepEqual(j[1].Postings, []Posting{live(SellerHoldback(seller), share), live(SellerAvailable(seller), -share)}) {
			t.Fatalf("after the rejection, the held use's journal = %+v; want a release of %d µUSD to available", j, share)
		}
		audited(c.ID, "market.ip_claim.file", "market.ip_claim.counter", "market.ip_claim.rejected")
	})

	t.Run("attributed writes a claim edge so the next sale pays the claimant", func(t *testing.T) {
		listing("lst_b3247_orig", claimant)
		listing("lst_b3247_b", seller)
		c, err := s.FileIPClaim(ctx, claimant, IPClaimFiling{ListingID: "lst_b3247_b", OriginalListingID: "lst_b3247_orig",
			Evidence: "built on my prompt", GoodFaith: "I believe in good faith that this copies my work."}, filed)
		if err != nil {
			t.Fatal(err)
		}
		attribute := IPClaimDecision{Outcome: ClaimAttributed, ShareBPS: 2000, Reason: "a remix that did not credit its original"}
		if _, err := s.DecideIPClaim(ctx, nil, c.ID, "ops@talyvor", attribute, filed.Add(24*time.Hour)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("deciding inside the seller's counter window = %v; want it refused", err)
		}
		if _, err := s.DecideIPClaim(ctx, nil, c.ID, "ops@talyvor", attribute, filed.Add(11*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		var share int
		var source string
		if err := pool.QueryRow(ctx, `SELECT share_bps, source FROM market_lineage WHERE child_listing_id = 'lst_b3247_b' AND child_version = 1
			AND parent_listing_id = 'lst_b3247_orig'`).Scan(&share, &source); err != nil || share != 2000 || source != LineageClaim {
			t.Fatalf("the attribution's edge = %d bps from %q, %v; want a claim edge at 2000", share, source, err)
		}
		// The next $20.00 sale: 15% to Talyvor, and 20% of the 17,000,000 left to the claimant.
		j := sale("use_b3247_after", "lst_b3247_b", filed.Add(12*24*time.Hour))
		want := []Posting{live(AccountStripeClearing, 20_000_000), live(AccountMarketFee, -3_000_000),
			live(SellerHoldback(seller), -13_600_000), live(SellerHoldback(claimant), -3_400_000)}
		if len(j) != 1 || !reflect.DeepEqual(j[0].Postings, want) {
			t.Fatalf("the sale after the attribution's journal = %+v; want %v", j, want)
		}
		audited(c.ID, "market.ip_claim.file", "market.ip_claim.attributed")
	})

	t.Run("upheld takes the listing down and refunds the held earning, past its 14 days", func(t *testing.T) {
		listing("lst_b3247_c", seller)
		c, err := s.FileIPClaim(ctx, claimant, IPClaimFiling{ListingID: "lst_b3247_c", OriginalReference: "https://example.com/another",
			Evidence: "a copy", GoodFaith: "I believe in good faith that this copies my work."}, filed)
		if err != nil {
			t.Fatal(err)
		}
		held := sale("use_b3247_upheld", "lst_b3247_c", filed.Add(time.Hour))
		out, err := s.DecideIPClaim(ctx, nil, c.ID, "ops@talyvor", IPClaimDecision{Outcome: ClaimUpheld, Reason: "a copy"}, filed.Add(20*24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if out.Takedown == nil || len(out.Takedown.Refunds) != 1 || out.Takedown.Refunds[0].UseID != "use_b3247_upheld" || out.HoldsReleased != 1 {
			t.Fatalf("the upheld claim = %+v; want the listing down, its held use refunded and its hold released", out)
		}
		j := journal(t, s, "use_b3247_upheld")
		if len(j) != 2 || j[1].Kind != JournalReversal || j[1].Postings[2] != live(SellerHoldback(seller), 17_000_000) ||
			j[0].Postings[2] != held[0].Postings[2] {
			t.Fatalf("the upheld claim's held use's journal = %+v; want its clear and a reversal out of the holdback", j)
		}
		audited(c.ID, "market.ip_claim.file", "market.ip_claim.upheld")
	})

	t.Run("a claim deleted with its workspace releases its holds", func(t *testing.T) {
		listing("lst_b3247_d", seller)
		c, err := s.FileIPClaim(ctx, claimant, IPClaimFiling{ListingID: "lst_b3247_d", OriginalReference: "https://example.com/third",
			Evidence: "a copy", GoodFaith: "I believe in good faith that this copies my work."}, filed)
		if err != nil {
			t.Fatal(err)
		}
		sale("use_b3247_withdrawn", "lst_b3247_d", filed.Add(time.Hour))
		if _, err := pool.Exec(ctx, `DELETE FROM market_ip_claims WHERE claimant_workspace_id = $1 AND id = $2`, claimant, c.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReleaseDue(ctx, filed.Add(20*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if j := journal(t, s, "use_b3247_withdrawn"); len(j) != 2 || j[1].Kind != JournalRelease {
			t.Fatalf("after its claim was deleted, the held use's journal = %+v; want its clear and its release", j)
		}
	})
}

func journal(t *testing.T, s *Store, ref string) []JournalEntry {
	t.Helper()
	j, err := s.JournalFor(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	return j
}
