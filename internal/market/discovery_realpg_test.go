package market

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/talyvor/lens/internal/embedder"
)

// B32.50 — search pages past the old 500 cap, and a sentence is matched by any of its words and re-ranked by meaning.
func TestDiscover_PagesPast500AndRerankSentences(t *testing.T) {
	pool := migratedDB(t)
	ctx := context.Background()
	s := NewStore(pool)

	t.Run("520 listings read in eleven pages, the last with 20", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `INSERT INTO market_listings (id, workspace_id, kind, title, created_at)
			SELECT 'lst_b3250_page_' || n, 'ws-b3250-pager', 'skill', 'Pager ' || n, now() - n * interval '1 second' FROM generate_series(1, 520) n`); err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for page := 1; ; page++ {
			p, err := s.Discover(ctx, DiscoverQuery{Kind: "skill", Sort: SortNew, Page: page})
			if err != nil {
				t.Fatal(err)
			}
			if p.Total != 520 {
				t.Fatalf("page %d: total %d; want 520", page, p.Total)
			}
			for _, h := range p.Listings {
				seen[h.ID] = true
			}
			if !p.HasMore {
				if page != 11 || len(p.Listings) != 20 {
					t.Fatalf("the last page is %d with %d listings; want 11 with 20", page, len(p.Listings))
				}
				break
			}
		}
		if len(seen) != 520 {
			t.Fatalf("the pages read %d distinct listings; want all 520", len(seen))
		}
	})

	t.Run("a sentence matches any of its words and is re-ranked by meaning", func(t *testing.T) {
		publish := func(title, template string) string {
			t.Helper()
			artifact, _ := json.Marshal(map[string]string{"template": template})
			l, err := s.Publish(ctx, "ws-b3250-seller-"+title[:4], Draft{Kind: "prompt", Title: title, Artifact: artifact})
			if err != nil {
				t.Fatal(err)
			}
			return l.ID
		}
		// The poem's title carries every word of the query; the reader's carries one, but all of its text is about them.
		s.SetSimilarity(embedder.NewHashedEmbedder(), 1.01) // fingerprint every version, never hold one
		reader := publish("Invoice reader", "Read the invoice and list the vendor, the totals and the dates.")
		poem := publish("Poem of invoice vendor totals dates", "Write a long poem about the sea, the moon, the stars, the wind, the waves and the night sky.")
		const sentence = "list the vendor totals and dates of an invoice"
		order := func() []string {
			t.Helper()
			p, err := s.Discover(ctx, DiscoverQuery{Q: sentence})
			if err != nil {
				t.Fatal(err)
			}
			ids := []string{}
			for _, h := range p.Listings {
				ids = append(ids, h.ID)
			}
			return ids
		}
		if got := order(); len(got) != 2 || got[0] != reader || got[1] != poem {
			t.Fatalf("with the embedder = %v; want the reader (nearest in meaning) before the poem", got)
		}
		s.similarity = nil
		if got := order(); len(got) != 2 || got[0] != poem || got[1] != reader {
			t.Fatalf("by text rank alone = %v; want the poem (every word in its title) before the reader", got)
		}
		if p, err := s.Discover(ctx, DiscoverQuery{Q: "invoice dates"}); err != nil || len(p.Listings) != 1 || p.Listings[0].ID != poem {
			t.Fatalf("two words, both required = %+v, %v; want the poem alone", p.Listings, err)
		}
	})
}
