package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/market"
)

// B32.50 — uses from a seller's linked workspace do not move its trending rank; a search for capability extract under
// $0.05 a use returns only listings with that capability and a per-use price at or under it; a public collection lists
// exactly its listings, and a featured one is listed first.
func TestMarketDiscovery_TrendingSearchAndCollections(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	store := market.NewStore(pool)
	r := chi.NewRouter()
	mountMarketRoutes(r, store)
	mountMarketDiscoveryRoutes(r, store)
	r.Method(http.MethodPost, "/v1/admin/marketplace/collections/{collectionID}/feature", newMarketFeatureCollectionHandler(store))
	call := func(ws, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set(moderatorOperatorHeader, "nicolai")
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	const sellerA, sellerB, buyer1, buyer2, buyer3, curator, talyvor = "ws-b3250-a", "ws-b3250-b", "ws-b3250-b1", "ws-b3250-b2", "ws-b3250-b3", "ws-b3250-cur", "ws-b3250-talyvor"
	linkedWs := []string{"ws-b3250-l1", "ws-b3250-l2", "ws-b3250-l3"}
	for _, ws := range append([]string{sellerA, sellerB, buyer1, buyer2, buyer3, curator, talyvor}, linkedWs...) {
		exec(`INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws)
	}
	publish := func(seller, title, offers string, caps ...string) string {
		t.Helper()
		capsJSON, _ := json.Marshal(append([]string{}, caps...))
		code, body := call(seller, http.MethodPost, "/v1/workspaces/"+seller+"/marketplace/listings",
			fmt.Sprintf(`{"kind":"prompt","title":%q,"description":"b3250","artifact":{"template":"%s {{text}}"},"offers":%s,"capabilities":%s}`,
				title, title, offers, capsJSON))
		var l market.Listing
		if err := json.Unmarshal([]byte(body), &l); code != http.StatusCreated || err != nil {
			t.Fatalf("publish %s = %d %s", title, code, body)
		}
		return l.ID
	}
	perUse := func(usdMicros int) string {
		return fmt.Sprintf(`[{"kind":"per_use","licence":"commercial","price_usd_micros":%d}]`, usdMicros)
	}
	search := func(query string) market.DiscoverPage {
		t.Helper()
		code, body := call(buyer1, http.MethodGet, "/v1/marketplace/search?"+query, "")
		var p market.DiscoverPage
		if err := json.Unmarshal([]byte(body), &p); code != http.StatusOK || err != nil {
			t.Fatalf("search %s = %d %s", query, code, body)
		}
		return p
	}
	ids := func(p market.DiscoverPage) []string {
		out := []string{}
		for _, h := range p.Listings {
			out = append(out, h.ID)
		}
		return out
	}

	t.Run("a search for capability extract under $0.05 a use returns only those", func(t *testing.T) {
		cheap := publish(sellerA, "Invoice extractor", perUse(40_000), "extract", "finance")
		atPrice := publish(sellerB, "Clause extractor", perUse(50_000), "extract", "legal")
		free := publish(sellerB, "Free extractor", `[]`, "extract")
		dear := publish(sellerA, "Premium extractor", perUse(60_000), "extract")
		other := publish(sellerA, "Cheap summarizer", perUse(10_000), "summarize")
		boughtOnly := publish(sellerB, "Bought extractor", `[{"kind":"buy","licence":"commercial","price_usd_micros":1000}]`)
		if code, body := call(sellerB, http.MethodPut, "/v1/workspaces/"+sellerB+"/marketplace/listings/"+boughtOnly+"/capabilities",
			`{"capabilities":["extract"]}`); code != http.StatusOK || !strings.Contains(body, `"extract"`) {
			t.Fatalf("setting its capabilities = %d %s", code, body)
		}
		if code, body := call(sellerB, http.MethodPut, "/v1/workspaces/"+sellerB+"/marketplace/listings/"+boughtOnly+"/capabilities",
			`{"capabilities":["telepathy"]}`); code != http.StatusBadRequest || !strings.Contains(body, "summarize") {
			t.Fatalf("a capability off the list = %d %s; want 400 naming the list", code, body)
		}
		p := search("capability=extract&max_price_per_use=50000&sort=price")
		if got, want := ids(p), []string{free, cheap, atPrice}; !slices.Equal(got, want) {
			t.Fatalf("extract under $0.05 = %v; want %v (free, $0.04, $0.05 — not %s, %s or %s)", got, want, dear, other, boughtOnly)
		}
		for _, h := range p.Listings {
			if !slices.Contains(h.Capabilities, "extract") || h.PricePerUseUSDMicros == nil || *h.PricePerUseUSDMicros > 50_000 {
				t.Fatalf("hit %s: capabilities %v, price per use %v", h.ID, h.Capabilities, h.PricePerUseUSDMicros)
			}
		}
		if p.Total != 3 || p.HasMore {
			t.Fatalf("total %d, has_more %v; want 3, false", p.Total, p.HasMore)
		}
		if got := ids(search("q=invoice&capability=extract")); !slices.Equal(got, []string{cheap}) {
			t.Fatalf("q=invoice among extract = %v; want the invoice extractor", got)
		}
	})

	t.Run("uses from a seller's linked workspace do not move its trending rank", func(t *testing.T) {
		listingA := publish(sellerA, "Trend A", perUse(1_000))
		listingB := publish(sellerB, "Trend B", perUse(1_000))
		n := 0
		use := func(listing, seller, buyer, charge string, at time.Time) {
			t.Helper()
			n++
			exec(`INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, price_ulxc, charge, used_at, ran_at)
				VALUES ($1, $2, 1, $3, $4, CASE WHEN $5 = 'billed' THEN 10000 ELSE 0 END, $5, $6, $6)`,
				fmt.Sprintf("use_b3250_%d", n), listing, seller, buyer, charge, at)
		}
		now := time.Now()
		// B has two real buyers today; A has one today and one two days ago.
		use(listingB, sellerB, buyer1, "billed", now)
		use(listingB, sellerB, buyer2, "billed", now)
		use(listingA, sellerA, buyer3, "billed", now)
		use(listingA, sellerA, buyer1, "billed", now.AddDate(0, 0, -2))
		trending := func() ([]string, map[string]market.DiscoverHit) {
			t.Helper()
			if _, err := store.RefreshDiscoveryStats(ctx, now); err != nil {
				t.Fatal(err)
			}
			p := search("sort=trending")
			order, hits := []string{}, map[string]market.DiscoverHit{}
			for _, h := range p.Listings {
				if h.ID == listingA || h.ID == listingB {
					order = append(order, h.ID)
					hits[h.ID] = h
				}
			}
			return order, hits
		}
		before, hitsBefore := trending()
		if !slices.Equal(before, []string{listingB, listingA}) || hitsBefore[listingA].DistinctBuyers7d != 2 || hitsBefore[listingB].DistinctBuyers7d != 2 {
			t.Fatalf("before = %v %+v; want B (2 buyers today) ahead of A (one today, one two days ago)", before, hitsBefore)
		}
		// Three workspaces that share the seller's card use A every day: one charged as linked, two found linked only
		// after they were charged. The seller uses its own listing too.
		for _, ws := range linkedWs {
			exec(`INSERT INTO workspace_card_fingerprints (workspace_id, fingerprint_hash) VALUES ($1, 'card-b3250')`, ws)
		}
		exec(`INSERT INTO workspace_card_fingerprints (workspace_id, fingerprint_hash) VALUES ($1, 'card-b3250')`, sellerA)
		for d := 0; d < market.TrendingDays; d++ {
			use(listingA, sellerA, linkedWs[0], "linked", now.AddDate(0, 0, -d))
			use(listingA, sellerA, linkedWs[1], "billed", now.AddDate(0, 0, -d))
			use(listingA, sellerA, linkedWs[2], "billed", now.AddDate(0, 0, -d))
		}
		use(listingA, sellerA, sellerA, "own", now)
		after, hitsAfter := trending()
		if !slices.Equal(after, before) || hitsAfter[listingA].TrendingScore != hitsBefore[listingA].TrendingScore || hitsAfter[listingA].DistinctBuyers7d != 2 {
			t.Fatalf("after the linked uses = %v %+v; want the rank and A's score unchanged: %v %+v", after, hitsAfter, before, hitsBefore)
		}
		var uses, buyers int
		var revenue int64
		if err := pool.QueryRow(ctx, `SELECT coalesce(sum(uses), 0), coalesce(sum(distinct_buyers), 0), coalesce(sum(revenue_usd_micros), 0)
			FROM market_listing_stats WHERE listing_id = $1`, listingA).Scan(&uses, &buyers, &revenue); err != nil {
			t.Fatal(err)
		}
		if uses != 2 || buyers != 2 || revenue != 2_000 {
			t.Fatalf("A's daily stats sum to %d uses, %d buyers, %d µUSD; want 2, 2 and 2000 (the linked and own uses left out)", uses, buyers, revenue)
		}
		// A third real buyer today does move it: A passes B.
		use(listingA, sellerA, buyer2, "billed", now)
		if moved, _ := trending(); !slices.Equal(moved, []string{listingA, listingB}) {
			t.Fatalf("after a real buyer = %v; want A ahead of B", moved)
		}
	})

	t.Run("a public collection lists exactly its listings, and a featured one is listed first", func(t *testing.T) {
		one, two, three := publish(sellerA, "Coll one", `[]`), publish(sellerB, "Coll two", `[]`), publish(sellerA, "Coll three", `[]`)
		hidden := publish(sellerA, "Coll hidden", `[]`)
		exec(`UPDATE market_listings SET visibility = 'private' WHERE id = $1`, hidden)
		save := func(ws, method, path, body string) market.Collection {
			t.Helper()
			code, out := call(ws, method, path, body)
			var c market.Collection
			if err := json.Unmarshal([]byte(out), &c); (code != http.StatusCreated && code != http.StatusOK) || err != nil {
				t.Fatalf("%s %s = %d %s", method, path, code, out)
			}
			return c
		}
		if code, body := call(curator, http.MethodPost, "/v1/workspaces/"+curator+"/marketplace/collections",
			fmt.Sprintf(`{"title":"Mine","public":true,"listing_ids":[%q]}`, hidden)); code != http.StatusBadRequest {
			t.Fatalf("a collection of a private listing = %d %s; want 400", code, body)
		}
		talyvors := save(talyvor, http.MethodPost, "/v1/workspaces/"+talyvor+"/marketplace/collections",
			fmt.Sprintf(`{"title":"Talyvor picks","public":true,"listing_ids":[%q]}`, two))
		curated := save(curator, http.MethodPost, "/v1/workspaces/"+curator+"/marketplace/collections",
			fmt.Sprintf(`{"title":"Best prompts","description":"three good ones","public":true,"listing_ids":[%q,%q,%q]}`, three, one, two))
		draft := save(curator, http.MethodPost, "/v1/workspaces/"+curator+"/marketplace/collections", `{"title":"Not yet","listing_ids":[]}`)

		code, body := call(buyer1, http.MethodGet, "/v1/marketplace/collections/"+curated.ID, "")
		var read market.Collection
		if err := json.Unmarshal([]byte(body), &read); code != http.StatusOK || err != nil {
			t.Fatalf("reading the collection = %d %s", code, body)
		}
		got := []string{}
		for _, l := range read.Listings {
			got = append(got, l.ID)
		}
		if want := []string{three, one, two}; !slices.Equal(got, want) || read.ListingCount != 3 {
			t.Fatalf("the collection lists %v (count %d); want exactly %v in its order", got, read.ListingCount, want)
		}
		if code, _ := call(buyer1, http.MethodGet, "/v1/marketplace/collections/"+draft.ID, ""); code != http.StatusNotFound {
			t.Fatalf("someone else reading a collection that is not public = %d; want 404", code)
		}

		public := func() []string {
			t.Helper()
			code, body := call(buyer1, http.MethodGet, "/v1/marketplace/collections", "")
			var out struct {
				Collections []market.Collection `json:"collections"`
			}
			if err := json.Unmarshal([]byte(body), &out); code != http.StatusOK || err != nil {
				t.Fatalf("the public collections = %d %s", code, body)
			}
			ids := []string{}
			for _, c := range out.Collections {
				ids = append(ids, c.ID)
			}
			return ids
		}
		if got := public(); !slices.Equal(got, []string{curated.ID, talyvors.ID}) {
			t.Fatalf("before featuring = %v; want the newest first and no private one", got)
		}
		if code, body := call("", http.MethodPost, "/v1/admin/marketplace/collections/"+draft.ID+"/feature", `{"featured":true}`); code != http.StatusBadRequest {
			t.Fatalf("featuring a collection that is not public = %d %s; want 400", code, body)
		}
		if code, body := call("", http.MethodPost, "/v1/admin/marketplace/collections/"+talyvors.ID+"/feature", `{"featured":true}`); code != http.StatusOK ||
			!strings.Contains(body, `"featured":true`) {
			t.Fatalf("featuring Talyvor's collection = %d %s", code, body)
		}
		if got := public(); !slices.Equal(got, []string{talyvors.ID, curated.ID}) {
			t.Fatalf("after featuring = %v; want Talyvor's featured one first", got)
		}
		var audited int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM operator_audit WHERE action = 'market.collection.feature' AND target = $1 AND actor = 'nicolai'`,
			talyvors.ID).Scan(&audited); err != nil || audited != 1 {
			t.Fatalf("the operator audit rows for the feature = %d, %v; want 1", audited, err)
		}
	})
}
