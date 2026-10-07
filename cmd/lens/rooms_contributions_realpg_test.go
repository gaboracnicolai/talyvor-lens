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
	"github.com/talyvor/lens/internal/rooms"
)

// B32.31 — a contribution is visible to the room's members, 404 to others and absent from the public catalog; a fork
// records a room_fork edge at the room's remix share; a member's second vote replaces the first and the tally is the
// sum of the latest votes.
func TestRooms_ContributionsProposeForkWithLineageAndVote(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const owner, member, outsider = "ws-b3231-owner", "ws-b3231-member", "ws-b3231-outsider"
	marketStore := market.NewStore(pool)
	roomStore := rooms.NewStore(pool, 3000)
	roomStore.SetMarket(marketStore)
	r := chi.NewRouter()
	mountMarketRoutes(r, marketStore)
	mountRoomRoutes(r, roomStore)
	call := func(ws, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "user-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	// do calls and decodes the answer into v, which must have the status want.
	do := func(want int, v any, ws, method, path, body string) {
		t.Helper()
		code, out := call(ws, method, path, body)
		if err := json.Unmarshal([]byte(out), v); code != want || err != nil {
			t.Fatalf("%s %s = %d %s, want %d", method, path, code, out, want)
		}
	}

	var room rooms.Detail
	do(http.StatusCreated, &room, owner, http.MethodPost, "/v1/workspaces/"+owner+"/rooms",
		`{"title":"French product copy","terms":{"remix_share_bps":1200,"default_price_usd_micros":20000}}`)
	if code, body := call(member, http.MethodPost, "/v1/rooms/"+room.ID+"/join", `{"terms_version":1}`); code != http.StatusCreated {
		t.Fatalf("join = %d %s", code, body)
	}

	// The owner proposes a prompt: a listing at the room's default price, announced to the room.
	var c rooms.Contribution
	do(http.StatusCreated, &c, owner, http.MethodPost, "/v1/rooms/"+room.ID+"/contributions",
		`{"kind":"prompt","title":"Tone guide","artifact":{"template":"Rewrite {{text}} in a warm, plain French."}}`)
	if c.Status != rooms.ContributionProposed || c.AuthorWorkspaceID != owner || c.ListingID == "" || c.MessageID == "" {
		t.Fatalf("contribution = %+v", c)
	}
	var price int64
	if err := pool.QueryRow(ctx, `SELECT price_usd_micros FROM market_offers WHERE listing_id = $1 AND kind = 'per_use'`,
		c.ListingID).Scan(&price); err != nil || price != 20000 {
		t.Fatalf("the contribution's per_use price = %d (%v), want the room's default 20000", price, err)
	}
	if _, body := call(member, http.MethodGet, "/v1/rooms/"+room.ID+"/messages", ""); !strings.Contains(body, `"kind":"contribution"`) ||
		!strings.Contains(body, c.ID) {
		t.Fatalf("the room's messages = %s, want the contribution message", body)
	}

	// A member sees the listing and opens its artifact; anyone else gets 404; the public catalog never lists it.
	if code, body := call(member, http.MethodGet, "/v1/marketplace/listings/"+c.ListingID, ""); code != http.StatusOK ||
		!strings.Contains(body, `"visibility":"room"`) || !strings.Contains(body, "warm, plain French") {
		t.Fatalf("the listing read by a member = %d %s, want 200 with its artifact", code, body)
	}
	if code, body := call(outsider, http.MethodGet, "/v1/marketplace/listings/"+c.ListingID, ""); code != http.StatusNotFound {
		t.Fatalf("the listing read by a non-member = %d %s, want 404", code, body)
	}
	if code, body := call(outsider, http.MethodGet, "/v1/rooms/"+room.ID+"/contributions/"+c.ID, ""); code == http.StatusOK {
		t.Fatalf("the contribution read by a non-member = %d %s, want it refused", code, body)
	}
	if code, body := call(outsider, http.MethodGet, "/v1/marketplace/listings", ""); code != http.StatusOK || strings.Contains(body, c.ListingID) {
		t.Fatalf("the public catalog = %d %s, want it without the room's listing", code, body)
	}

	// The member forks it: its own contribution, whose lineage is a room_fork edge at the room's remix share.
	var fork rooms.Contribution
	do(http.StatusCreated, &fork, member, http.MethodPost, "/v1/rooms/"+room.ID+"/contributions/"+c.ID+"/fork",
		`{"title":"Tone guide, formal","artifact":{"template":"Rewrite {{text}} in a formal French."}}`)
	if fork.ForkedFrom != c.ID || fork.AuthorWorkspaceID != member || fork.Kind != "prompt" {
		t.Fatalf("fork = %+v, want the member's prompt forked from %s", fork, c.ID)
	}
	var source string
	var share, parentVersion int
	if err := pool.QueryRow(ctx, `SELECT source, share_bps, parent_version FROM market_lineage WHERE child_listing_id = $1 AND parent_listing_id = $2`,
		fork.ListingID, c.ListingID).Scan(&source, &share, &parentVersion); err != nil {
		t.Fatalf("the fork's lineage edge: %v", err)
	}
	if source != market.LineageRoomFork || share != 1200 || parentVersion != 1 {
		t.Fatalf("the fork's edge = %s at %d bps from v%d, want room_fork at the room's 1200 from v1", source, share, parentVersion)
	}
	// A non-member cannot fork it.
	if code, _ := call(outsider, http.MethodPost, "/v1/rooms/"+room.ID+"/contributions/"+c.ID+"/fork", `{}`); code == http.StatusCreated {
		t.Fatalf("a non-member's fork = %d, want it refused", code)
	}

	// Votes: the member's second vote replaces its first; the tally is the sum of each member's latest vote.
	vote := func(ws string, value string) rooms.Contribution {
		t.Helper()
		var out rooms.Contribution
		do(http.StatusOK, &out, ws, http.MethodPut, "/v1/rooms/"+room.ID+"/contributions/"+c.ID+"/vote", `{"value":`+value+`}`)
		return out
	}
	vote(member, "1")
	vote(owner, "1")
	if got := vote(member, "-1"); got.Tally != 0 || got.Up != 1 || got.Down != 1 || got.MyVote != -1 {
		t.Fatalf("after the member changed its vote = tally %d (+%d -%d, mine %d), want 0 (+1 -1, mine -1)", got.Tally, got.Up, got.Down, got.MyVote)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM room_votes WHERE contribution_id = $1 AND workspace_id = $2`, c.ID, member).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("the member's vote rows = %d (%v), want 1", rows, err)
	}
	if code, _ := call(outsider, http.MethodPut, "/v1/rooms/"+room.ID+"/contributions/"+c.ID+"/vote", `{"value":1}`); code != http.StatusForbidden {
		t.Fatalf("a non-member's vote = %d, want 403", code)
	}

	// The owner or an editor accepts or rejects; a member does not.
	if code, _ := call(member, http.MethodPatch, "/v1/rooms/"+room.ID+"/contributions/"+c.ID, `{"status":"accepted"}`); code != http.StatusForbidden {
		t.Fatalf("a member accepting = %d, want 403", code)
	}
	var accepted rooms.Contribution
	do(http.StatusOK, &accepted, owner, http.MethodPatch, "/v1/rooms/"+room.ID+"/contributions/"+c.ID, `{"status":"accepted"}`)
	if accepted.Status != rooms.ContributionAccepted || accepted.DecidedByWorkspaceID != owner {
		t.Fatalf("accepted = %+v", accepted)
	}
	var list struct {
		Contributions []rooms.Contribution `json:"contributions"`
	}
	do(http.StatusOK, &list, member, http.MethodGet, "/v1/rooms/"+room.ID+"/contributions", "")
	if len(list.Contributions) != 2 || list.Contributions[0].ID != fork.ID || list.Contributions[1].Status != rooms.ContributionAccepted {
		t.Fatalf("the room's contributions = %+v, want the fork then the accepted original", list.Contributions)
	}
}
