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
	"github.com/talyvor/lens/internal/rooms"
)

// B32.28 — a created room has its creator as owner; another workspace joins under the current terms version and that
// version is recorded; a new terms version asks it again; a viewer cannot be given may_spend; an agent joins only as
// its owner's member; GET /v1/rooms lists an open public room and not a closed one.
func TestRooms_CreateJoinUnderTermsRolesAndTheOpenList(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const owner, joiner, outsider = "ws-room-owner", "ws-room-joiner", "ws-room-outsider"
	r := chi.NewRouter()
	mountRoomRoutes(r, rooms.NewStore(pool, 3000))
	call := func(ws, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "user-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	create := func(title string) rooms.Detail {
		t.Helper()
		code, body := call(owner, http.MethodPost, "/v1/workspaces/"+owner+"/rooms",
			`{"title":"`+title+`","topic":"Translation","terms":{"split_rule":"equal","remix_share_bps":1000,"default_price_usd_micros":20000,"spend_policy":"members_with_spend"}}`)
		var d rooms.Detail
		if err := json.Unmarshal([]byte(body), &d); code != http.StatusCreated || err != nil {
			t.Fatalf("create %q = %d %s", title, code, body)
		}
		return d
	}
	row := func(roomID, ws string) (role string, maySpend bool, terms int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT role, may_spend, terms_version FROM room_members WHERE room_id = $1 AND workspace_id = $2`,
			roomID, ws).Scan(&role, &maySpend, &terms); err != nil {
			t.Fatalf("member row %s/%s: %v", roomID, ws, err)
		}
		return role, maySpend, terms
	}

	// The creator is the owner, with the room's budget, at terms version 1.
	room := create("French product copy")
	if room.OwnerWorkspaceID != owner || room.Me == nil || room.Me.Role != rooms.RoleOwner || room.Terms.Version != 1 ||
		room.Terms.SplitRule != rooms.SplitEqual || room.Terms.DefaultPriceUSDMicros != 20000 {
		t.Fatalf("created room = %+v, want its creator as owner at terms version 1", room)
	}
	if role, maySpend, terms := row(room.ID, owner); role != "owner" || !maySpend || terms != 1 {
		t.Fatalf("owner row = %s %v v%d, want owner, may_spend, v1", role, maySpend, terms)
	}

	// Joining must accept the current terms: a wrong version is 409 and writes no row; the right one records it.
	if code, body := call(joiner, http.MethodPost, "/v1/rooms/"+room.ID+"/join", `{"terms_version":2}`); code != http.StatusConflict {
		t.Fatalf("join with terms v2 = %d %s, want 409", code, body)
	}
	if code, body := call(joiner, http.MethodPost, "/v1/rooms/"+room.ID+"/join", `{"terms_version":1}`); code != http.StatusCreated ||
		!strings.Contains(body, `"terms_version":1`) || !strings.Contains(body, `"role":"member"`) {
		t.Fatalf("join = %d %s, want 201 as a member at terms v1", code, body)
	}
	if role, maySpend, terms := row(room.ID, joiner); role != "member" || maySpend || terms != 1 {
		t.Fatalf("joiner row = %s %v v%d, want member without budget at v1", role, maySpend, terms)
	}

	// New terms ask the member again: its row keeps v1 and reads not current until it joins under v2.
	if code, body := call(owner, http.MethodPut, "/v1/rooms/"+room.ID+"/terms",
		`{"split_rule":"by_votes","remix_share_bps":1000,"default_price_usd_micros":20000,"spend_policy":"members_with_spend"}`); code != http.StatusOK ||
		!strings.Contains(body, `"version":2`) {
		t.Fatalf("new terms = %d %s, want version 2", code, body)
	}
	if _, body := call(joiner, http.MethodGet, "/v1/rooms/"+room.ID, ""); !strings.Contains(body, `"terms_version":1,"terms_current":false`) {
		t.Fatalf("the joiner after new terms = %s, want its v1 acceptance marked not current", body)
	}
	if code, body := call(joiner, http.MethodPost, "/v1/rooms/"+room.ID+"/join", `{"terms_version":2}`); code != http.StatusOK {
		t.Fatalf("accept v2 = %d %s, want 200", code, body)
	}
	if _, _, terms := row(room.ID, joiner); terms != 2 {
		t.Fatalf("joiner terms after accepting = v%d, want v2", terms)
	}

	// A viewer cannot be given may_spend; a member can, and becoming a viewer takes it away.
	if code, body := call(owner, http.MethodPatch, "/v1/rooms/"+room.ID+"/members/"+joiner, `{"role":"viewer","may_spend":true}`); code != http.StatusBadRequest ||
		!strings.Contains(body, "a viewer cannot be given may_spend") {
		t.Fatalf("viewer with may_spend = %d %s, want 400", code, body)
	}
	if code, body := call(owner, http.MethodPatch, "/v1/rooms/"+room.ID+"/members/"+joiner, `{"may_spend":true}`); code != http.StatusOK {
		t.Fatalf("member may_spend = %d %s", code, body)
	}
	if code, body := call(owner, http.MethodPatch, "/v1/rooms/"+room.ID+"/members/"+joiner, `{"role":"viewer"}`); code != http.StatusOK {
		t.Fatalf("make viewer = %d %s", code, body)
	}
	if role, maySpend, _ := row(room.ID, joiner); role != "viewer" || maySpend {
		t.Fatalf("viewer row = %s may_spend %v, want a viewer without the budget", role, maySpend)
	}
	// Only the owner or an editor changes members.
	if code, _ := call(joiner, http.MethodPatch, "/v1/rooms/"+room.ID+"/members/"+owner, `{"role":"member"}`); code != http.StatusBadRequest {
		t.Fatalf("a viewer changing the owner = %d, want 400", code)
	}

	// An agent joins only as its owner's member.
	if _, err := pool.Exec(ctx, `INSERT INTO agent_accounts (id, workspace_id, name) VALUES ('agt_room_out', $1, 'Outsider bot')`, outsider); err != nil {
		t.Fatal(err)
	}
	if code, body := call(outsider, http.MethodPost, "/v1/rooms/"+room.ID+"/agents", `{"agent_id":"agt_room_out"}`); code != http.StatusForbidden {
		t.Fatalf("a non-member's agent = %d %s, want 403", code, body)
	}
	if code, body := call(outsider, http.MethodPost, "/v1/rooms/"+room.ID+"/join", `{"terms_version":2}`); code != http.StatusCreated {
		t.Fatalf("outsider join = %d %s", code, body)
	}
	if code, body := call(owner, http.MethodPost, "/v1/rooms/"+room.ID+"/agents", `{"agent_id":"agt_room_out"}`); code != http.StatusNotFound {
		t.Fatalf("someone else's agent = %d %s, want 404", code, body)
	}
	if code, body := call(outsider, http.MethodPost, "/v1/rooms/"+room.ID+"/agents", `{"agent_id":"agt_room_out"}`); code != http.StatusCreated ||
		!strings.Contains(body, `"workspace_id":"`+outsider+`"`) {
		t.Fatalf("its owner's agent = %d %s, want 201", code, body)
	}

	// GET /v1/rooms lists the open public room and not a closed one; the rooms you are in come beside it.
	closed := create("Closed room")
	if _, err := pool.Exec(ctx, `UPDATE rooms SET status = 'closed' WHERE id = $1`, closed.ID); err != nil {
		t.Fatal(err)
	}
	code, body := call(outsider, http.MethodGet, "/v1/rooms?topic=translation", "")
	var list struct {
		Rooms  []rooms.Room `json:"rooms"`
		Joined []rooms.Room `json:"joined"`
	}
	if err := json.Unmarshal([]byte(body), &list); code != http.StatusOK || err != nil {
		t.Fatalf("list = %d %s", code, body)
	}
	if len(list.Rooms) != 1 || list.Rooms[0].ID != room.ID || list.Rooms[0].MemberCount != 3 {
		t.Fatalf("open rooms = %+v, want only %s with its 3 members", list.Rooms, room.ID)
	}
	if len(list.Joined) != 1 || list.Joined[0].ID != room.ID {
		t.Fatalf("joined = %+v, want %s", list.Joined, room.ID)
	}
	if code, _ := call(joiner, http.MethodPost, "/v1/rooms/"+closed.ID+"/join", `{"terms_version":1}`); code != http.StatusConflict {
		t.Fatalf("join a closed room = %d, want 409", code)
	}
}
