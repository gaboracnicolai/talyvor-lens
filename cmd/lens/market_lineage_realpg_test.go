package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/market"
)

// B32.24 — publishing B with parent A (royalty 10%) records an edge at 1000 bps; raising A's share to 20% leaves B's
// edge at 1000, and B's next version keeps it; declaring B a parent of A is refused as a cycle; declaring a parent
// whose policy is none is refused; the lineage read returns A as B's parent, and A's remix count.
func TestMarketLineage_ParentsLockTheirShareAndRefuseCyclesAndClosedListings(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const sellerA, sellerB, sellerC, buyer = "ws-lin-a", "ws-lin-b", "ws-lin-c", "ws-lin-buyer"
	r := chi.NewRouter()
	mountMarketRoutes(r, market.NewStore(pool))
	call := func(ws, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	publish := func(ws, title, terms string) market.Listing {
		t.Helper()
		code, body := call(ws, http.MethodPost, "/v1/workspaces/"+ws+"/marketplace/listings",
			`{"kind":"prompt","title":"`+title+`","artifact":{"template":"Summarise {{text}}."}`+terms+`}`)
		var l market.Listing
		if err := json.Unmarshal([]byte(body), &l); code != http.StatusCreated || err != nil {
			t.Fatalf("publish %s = %d %s", title, code, body)
		}
		return l
	}
	edges := func(child string) map[int]int { // child version → share_bps of its one edge
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT child_version, share_bps FROM market_lineage WHERE child_listing_id = $1`, child)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[int]int{}
		for rows.Next() {
			var v, share int
			if err := rows.Scan(&v, &share); err != nil {
				t.Fatal(err)
			}
			out[v] = share
		}
		return out
	}

	a := publish(sellerA, "Original", `,"remix_policy":"royalty","remix_share_bps":1000`)
	if a.RemixPolicy != market.RemixRoyalty || a.RemixShareBPS != 1000 {
		t.Fatalf("A's terms = %s %d, want royalty 1000", a.RemixPolicy, a.RemixShareBPS)
	}
	b := publish(sellerB, "Remix", `,"remix_policy":"free","parents":[{"listing_id":"`+a.ID+`"}]`)
	if got := b.Versions[0].Parents; len(got) != 1 || got[0].ListingID != a.ID || got[0].Version != 1 || got[0].ShareBPS != 1000 {
		t.Fatalf("B's parents = %+v, want A v1 at 1000 bps", got)
	}
	if got := edges(b.ID); len(got) != 1 || got[1] != 1000 {
		t.Fatalf("B's edges = %v, want v1 at 1000 bps", got)
	}

	// A's owner raises the share: B's edge keeps the share it was declared with, and so does B's next version.
	if code, body := call(sellerA, http.MethodPut, "/v1/workspaces/"+sellerA+"/marketplace/listings/"+a.ID+"/remix-terms",
		`{"remix_policy":"royalty","remix_share_bps":2000}`); code != http.StatusOK {
		t.Fatalf("raise A's share = %d %s", code, body)
	}
	if code, body := call(sellerA, http.MethodPut, "/v1/workspaces/"+sellerA+"/marketplace/listings/"+a.ID+"/remix-terms",
		`{"remix_policy":"royalty","remix_share_bps":3001}`); code != http.StatusBadRequest {
		t.Fatalf("a share above LENS_LINEAGE_MAX_SHARE_BPS = %d %s, want 400", code, body)
	}
	if code, body := call(sellerB, http.MethodPost, "/v1/workspaces/"+sellerB+"/marketplace/listings/"+b.ID+"/versions",
		`{"artifact":{"template":"Summarise {{text}} in one line."},"changelog":"shorter"}`); code != http.StatusCreated {
		t.Fatalf("B v2 = %d %s", code, body)
	}
	if got := edges(b.ID); len(got) != 2 || got[1] != 1000 || got[2] != 1000 {
		t.Fatalf("B's edges after A's raise and B v2 = %v, want v1 and v2 at 1000 bps", got)
	}

	// A cannot build on its own remix.
	code, body := call(sellerA, http.MethodPost, "/v1/workspaces/"+sellerA+"/marketplace/listings/"+a.ID+"/versions",
		`{"artifact":{"template":"Summarise {{text}}, again."},"parents":[{"listing_id":"`+b.ID+`"}]}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "cannot build on one of its own remixes") {
		t.Fatalf("declare B a parent of A = %d %s, want 400 naming the cycle", code, body)
	}
	if got := edges(a.ID); len(got) != 0 {
		t.Fatalf("A's edges after the refused cycle = %v, want none", got)
	}

	// A listing that does not allow remixes cannot be declared as a parent by anyone but its owner.
	closed := publish(sellerC, "Closed", ``)
	code, body = call(sellerB, http.MethodPost, "/v1/workspaces/"+sellerB+"/marketplace/listings",
		`{"kind":"prompt","title":"Copy","artifact":{"template":"Summarise {{text}}!"},"parents":[{"listing_id":"`+closed.ID+`"}]}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "does not allow remixes") {
		t.Fatalf("declare a none-policy parent = %d %s, want 400", code, body)
	}
	var copies int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM market_listings WHERE title = 'Copy'`).Scan(&copies); err != nil || copies != 0 {
		t.Fatalf("listings titled Copy = %d (%v), want the refused publish to leave none", copies, err)
	}
	if got := edges(closed.ID); len(got) != 0 {
		t.Fatalf("edges into the closed listing = %v, want none", got)
	}

	// Anyone reads the family tree: A is B's parent at the share it was declared with, and A has one remix.
	code, body = call(buyer, http.MethodGet, "/v1/marketplace/listings/"+b.ID+"/lineage?version=1", "")
	var lin market.Lineage
	if err := json.Unmarshal([]byte(body), &lin); code != http.StatusOK || err != nil {
		t.Fatalf("B's lineage = %d %s", code, body)
	}
	if len(lin.Ancestors) != 1 || lin.Ancestors[0].ListingID != a.ID || lin.Ancestors[0].Title != "Original" ||
		lin.Ancestors[0].ShareBPS != 1000 || lin.Ancestors[0].Depth != 1 || lin.MaxDepth != market.DefaultLineageMaxDepth {
		t.Fatalf("B's lineage = %s, want A at 1000 bps, one generation up", body)
	}
	code, body = call(buyer, http.MethodGet, "/v1/marketplace/listings/"+a.ID+"/lineage", "")
	if err := json.Unmarshal([]byte(body), &lin); code != http.StatusOK || err != nil || len(lin.Ancestors) != 0 || lin.Descendants != 1 ||
		lin.ShareBPS != 2000 {
		t.Fatalf("A's lineage = %d %s, want no ancestors, one remix and its share now 2000", code, body)
	}
}
