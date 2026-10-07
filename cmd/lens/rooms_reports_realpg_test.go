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
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/rooms"
	"github.com/talyvor/lens/internal/tenant"
)

// B32.52 — three reports from different workspaces take a public room off GET /v1/rooms until the operator keeps it; a
// banned member's post and join are refused and a muted member's post is; a locked room is read-only; a closed room
// refuses a message and a run on its wallet, which posts nothing to the wallet's ledger; each operator action has its
// operator_audit row.
func TestRooms_ReportsHideARoom_BanMute_AndTheOperatorLocksAndCloses(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const owner, member, troll, quiet = "ws-b3252-owner", "ws-b3252-member", "ws-b3252-troll", "ws-b3252-quiet"
	reporters := []string{"ws-b3252-rep-a", "ws-b3252-rep-b", "ws-b3252-rep-c"}
	for _, ws := range append([]string{owner, member, troll, quiet}, reporters...) {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic, company) VALUES ($1, $1, $1, true, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 200000000, 200000000)`, owner); err != nil {
		t.Fatal(err)
	}
	bank := economy.NewDualTokenStore(nil, pool, nil)
	store := rooms.NewStore(pool, 3000)
	store.SetMarket(market.NewStore(pool))
	store.SetKeys(tenant.NewStore(pool))
	bank.SetRoomBudgets(store)
	noModel := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a closed room's run reached a model")
		w.WriteHeader(http.StatusInternalServerError)
	})
	var metered roomWalletMeter
	r := chi.NewRouter()
	mountRoomRoutes(r, store)
	mountAgentAccountRoutes(r, bank, tenant.NewStore(pool))
	mountRoomRunRoutes(r, store, noModel, noModel, &metered, bank)
	r.Post("/v1/admin/rooms/{roomID}/moderate", newRoomModerateHandler(store).ServeHTTP)
	call := func(ws, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if ws == "" {
			req.Header.Set(moderatorOperatorHeader, "ops@talyvor")
		} else {
			req = req.WithContext(auth.WithAuthContext(req.Context(),
				&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "user-" + ws, Scopes: []string{auth.ScopeKeys}}))
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	must := func(want int, ws, method, path, body string) string {
		t.Helper()
		code, out := call(ws, method, path, body)
		if code != want {
			t.Fatalf("%s %s by %q = %d %s, want %d", method, path, ws, code, out, want)
		}
		return out
	}
	listed := func(roomID string) bool {
		t.Helper()
		var out struct {
			Rooms []rooms.Room `json:"rooms"`
		}
		_ = json.Unmarshal([]byte(must(http.StatusOK, member, http.MethodGet, "/v1/rooms", "")), &out)
		for _, rm := range out.Rooms {
			if rm.ID == roomID {
				return true
			}
		}
		return false
	}

	var room rooms.Detail
	_ = json.Unmarshal([]byte(must(http.StatusCreated, owner, http.MethodPost, "/v1/workspaces/"+owner+"/rooms",
		`{"title":"Open launch room","topic":"Launch","terms":{"spend_policy":"members_with_spend"}}`)), &room)
	for _, ws := range []string{member, troll, quiet} {
		must(http.StatusCreated, ws, http.MethodPost, "/v1/rooms/"+room.ID+"/join", `{"terms_version":1}`)
	}
	var msg rooms.Message
	_ = json.Unmarshal([]byte(must(http.StatusCreated, troll, http.MethodPost, "/v1/rooms/"+room.ID+"/messages", `{"body":"buy my followers"}`)), &msg)

	// Two workspaces' reports — one of the room, one of a message — and the first one's again leave it listed; a third
	// workspace's takes it off until the operator keeps it.
	must(http.StatusCreated, reporters[0], http.MethodPost, "/v1/rooms/"+room.ID+"/reports", `{"reason":"spam","details":"follower sales"}`)
	must(http.StatusCreated, reporters[1], http.MethodPost, "/v1/rooms/"+room.ID+"/messages/"+msg.ID+"/reports", `{"reason":"harassment"}`)
	if out := must(http.StatusOK, reporters[0], http.MethodPost, "/v1/rooms/"+room.ID+"/reports", `{"reason":"spam"}`); !strings.Contains(out, `"already_reported":true`) {
		t.Fatalf("a second report by the same workspace = %s, want already_reported", out)
	}
	if code, out := call(reporters[2], http.MethodPost, "/v1/rooms/"+room.ID+"/reports", `{"reason":"rude"}`); code != http.StatusBadRequest {
		t.Fatalf("a report with an unknown reason = %d %s, want 400", code, out)
	}
	if !listed(room.ID) {
		t.Fatal("a room reported by two workspaces left the public list; it takes three")
	}
	must(http.StatusCreated, reporters[2], http.MethodPost, "/v1/rooms/"+room.ID+"/reports", `{"reason":"other"}`)
	if listed(room.ID) {
		t.Fatal("a room reported by three workspaces is still on GET /v1/rooms")
	}
	if out := must(http.StatusOK, owner, http.MethodGet, "/v1/rooms/"+room.ID, ""); !strings.Contains(out, `"under_review":true`) {
		t.Fatalf("the hidden room read by its owner = %s, want under_review", out)
	}
	if out := must(http.StatusOK, "", http.MethodPost, "/v1/admin/rooms/"+room.ID+"/moderate", `{"action":"keep"}`); !strings.Contains(out, `"resolved_reports":3`) {
		t.Fatalf("keep = %s, want its three open reports resolved", out)
	}
	if !listed(room.ID) {
		t.Fatal("a kept room is not back on GET /v1/rooms")
	}
	for _, ws := range reporters {
		must(http.StatusCreated, ws, http.MethodPost, "/v1/rooms/"+room.ID+"/reports", `{"reason":"spam"}`)
	}
	if !listed(room.ID) {
		t.Fatal("the reporters a room was kept against hid it again")
	}

	// The owner bans the troll: its post and its join are refused. It mutes quiet: its post is refused.
	must(http.StatusOK, owner, http.MethodPatch, "/v1/rooms/"+room.ID+"/members/"+troll, `{"banned":true}`)
	if code, out := call(troll, http.MethodPost, "/v1/rooms/"+room.ID+"/messages", `{"body":"still here"}`); code != http.StatusForbidden || !strings.Contains(out, "banned") {
		t.Fatalf("a banned member's post = %d %s, want 403 naming the ban", code, out)
	}
	if code, out := call(troll, http.MethodPost, "/v1/rooms/"+room.ID+"/join", `{"terms_version":1}`); code != http.StatusForbidden {
		t.Fatalf("a banned member's join = %d %s, want 403", code, out)
	}
	must(http.StatusOK, owner, http.MethodPatch, "/v1/rooms/"+room.ID+"/members/"+quiet, `{"muted":true}`)
	if code, out := call(quiet, http.MethodPost, "/v1/rooms/"+room.ID+"/messages", `{"body":"hello?"}`); code != http.StatusForbidden || !strings.Contains(out, "muted") {
		t.Fatalf("a muted member's post = %d %s, want 403 naming the mute", code, out)
	}
	if code, out := call(quiet, http.MethodPost, "/v1/rooms/"+room.ID+"/contributions",
		`{"kind":"prompt","title":"Muted","artifact":{"template":"x","model":"gpt-4o-mini"}}`); code != http.StatusForbidden {
		t.Fatalf("a muted member's contribution = %d %s, want 403", code, out)
	}

	// Locked, the room is read-only; unlocked, a member posts again.
	if code, out := call("", http.MethodPost, "/v1/admin/rooms/"+room.ID+"/moderate", `{"action":"lock"}`); code != http.StatusBadRequest {
		t.Fatalf("a lock without a reason = %d %s, want 400", code, out)
	}
	must(http.StatusOK, "", http.MethodPost, "/v1/admin/rooms/"+room.ID+"/moderate", `{"action":"lock","reason":"reported for spam"}`)
	if code, out := call(member, http.MethodPost, "/v1/rooms/"+room.ID+"/messages", `{"body":"anyone?"}`); code != http.StatusConflict || !strings.Contains(out, "read-only") {
		t.Fatalf("a post in a locked room = %d %s, want 409 read-only", code, out)
	}
	must(http.StatusOK, "", http.MethodPost, "/v1/admin/rooms/"+room.ID+"/moderate", `{"action":"unlock"}`)
	must(http.StatusCreated, member, http.MethodPost, "/v1/rooms/"+room.ID+"/messages", `{"body":"back again"}`)

	// The room's wallet is funded and the member may spend it; then the operator closes the room.
	wallet := room.Wallet.AgentID
	must(http.StatusOK, owner, http.MethodPost, "/v1/workspaces/"+owner+"/agents/"+wallet+"/fund", `{"amount_ulxc":50000000}`)
	must(http.StatusOK, owner, http.MethodPut, "/v1/workspaces/"+owner+"/agents/"+wallet+"/rules",
		`{"monthly_limit_ulxc":10000000,"max_per_request_ulxc":10000000,"approval_above_ulxc":10000000}`)
	must(http.StatusOK, owner, http.MethodPatch, "/v1/rooms/"+room.ID+"/members/"+member, `{"may_spend":true}`)
	var c rooms.Contribution
	_ = json.Unmarshal([]byte(must(http.StatusCreated, owner, http.MethodPost, "/v1/rooms/"+room.ID+"/contributions",
		`{"kind":"prompt","title":"Launch tagline","artifact":{"template":"A tagline for {{product}}.","model":"gpt-4o-mini"}}`)), &c)
	postings := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_postings WHERE workspace_id = $1 AND account = $2`, owner, "agent:"+wallet).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	funded := postings()
	if funded == 0 {
		t.Fatal("funding the room's wallet posted nothing to its ledger")
	}
	must(http.StatusOK, "", http.MethodPost, "/v1/admin/rooms/"+room.ID+"/moderate", `{"action":"close","reason":"abandoned after review"}`)
	if code, out := call(member, http.MethodPost, "/v1/rooms/"+room.ID+"/messages", `{"body":"hello?"}`); code != http.StatusConflict || !strings.Contains(out, "closed") {
		t.Fatalf("a post in a closed room = %d %s, want 409", code, out)
	}
	if code, out := call(member, http.MethodPost, "/v1/rooms/"+room.ID+"/runs",
		`{"target":"`+c.ID+`","variables":{"product":"Talyvor"},"pay":"room"}`); code != http.StatusConflict || !strings.Contains(out, "closed") {
		t.Fatalf("a run on a closed room's wallet = %d %s, want 409", code, out)
	}
	if n := postings(); n != funded {
		t.Fatalf("the closed room's wallet has %d postings after the refused run, want the %d of its funding", n, funded)
	}
	var uses int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM market_uses WHERE room_id = $1`, room.ID).Scan(&uses); err != nil || uses != 0 {
		t.Fatalf("market_uses of the closed room = %d (%v), want none", uses, err)
	}
	if code, out := call("", http.MethodPost, "/v1/admin/rooms/"+room.ID+"/moderate", `{"action":"unlock"}`); code != http.StatusConflict {
		t.Fatalf("unlocking a closed room = %d %s, want 409", code, out)
	}

	// Each operator action has its row in the trail, under the operator X-Talyvor-Operator named; the refused ones none.
	rows, err := pool.Query(ctx, `SELECT actor, action FROM operator_audit WHERE target = $1 ORDER BY id`, room.ID)
	if err != nil {
		t.Fatal(err)
	}
	var trail []string
	for rows.Next() {
		var actor, action string
		if err := rows.Scan(&actor, &action); err != nil {
			t.Fatal(err)
		}
		if actor != "ops@talyvor" {
			t.Fatalf("audit row %s by %q, want ops@talyvor", action, actor)
		}
		trail = append(trail, action)
	}
	rows.Close()
	if got := strings.Join(trail, ","); got != "room.keep,room.lock,room.unlock,room.close" {
		t.Fatalf("operator_audit for the room = %s, want room.keep,room.lock,room.unlock,room.close", got)
	}
}
