package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/rooms"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B32.28 — ROOMS: open chats where people and their agents build a product together (internal/rooms).
//
//	POST  /v1/workspaces/{wsID}/rooms        {title, topic, description, visibility, terms: {split_rule, remix_share_bps,
//	                                         default_price_usd_micros, spend_policy}}   the workspace opens a room it owns
//	GET   /v1/rooms?topic=                   {rooms, joined}: the open public rooms by latest activity — the list of open
//	                                         chats — and the rooms the caller's workspace is in
//	GET   /v1/rooms/{roomID}                 the room, its current terms, its members and agents, and the caller's membership
//	POST  /v1/rooms/{roomID}/join            {terms_version}   join, accepting the room's current terms
//	PUT   /v1/rooms/{roomID}/terms           {split_rule, remix_share_bps, default_price_usd_micros, spend_policy}   the owner
//	                                         writes a new terms version; each member is asked again before its next contribution
//	PATCH /v1/rooms/{roomID}/members/{ws}    {role, may_spend, remove}   by the owner or an editor; a member may remove itself
//	POST  /v1/rooms/{roomID}/agents          {agent_id}   one of the caller's agents joins, as the caller's member
//
// A private room answers 404 to everyone but its members. Joining with a terms_version that is not the room's current
// one, or joining a room that is locked or closed, is 409. Acting for a workspace in a room — opening, joining,
// changing members, bringing an agent — takes the workspace's owner or an admin, as publishing a listing does; reading
// takes any of its credentials.

func mountRoomRoutes(r chi.Router, store *rooms.Store) {
	writeErr := func(w http.ResponseWriter, err error) {
		switch {
		case errors.Is(err, rooms.ErrInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, rooms.ErrNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, rooms.ErrForbidden):
			writeJSONErr(w, http.StatusForbidden, err.Error())
		case errors.Is(err, rooms.ErrConflict):
			writeJSONErr(w, http.StatusConflict, err.Error())
		default:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		}
	}
	// actor is the workspace acting in a room, and the user acting for it; "" when the credential names no workspace.
	actor := func(w http.ResponseWriter, req *http.Request) (ws, user string, ok bool) {
		ws, _ = auth.WorkspaceIdentity(req.Context())
		if ws == "" {
			writeJSONErr(w, http.StatusForbidden, "acting in a room needs a workspace's credential")
			return "", "", false
		}
		if actx := auth.GetAuthContext(req.Context()); actx != nil {
			user = actx.UserID
		}
		return ws, user, true
	}
	status := func(created bool) int {
		if created {
			return http.StatusCreated
		}
		return http.StatusOK
	}

	r.Post("/v1/workspaces/{wsID}/rooms", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		var d rooms.Draft
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 32<<10)).Decode(&d); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be the room: "+err.Error())
			return
		}
		user := ""
		if actx := auth.GetAuthContext(req.Context()); actx != nil {
			user = actx.UserID
		}
		room, err := store.Create(req.Context(), chi.URLParam(req, "wsID"), user, d)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, room)
	}))
	r.Get("/v1/rooms", func(w http.ResponseWriter, req *http.Request) {
		ws, _ := auth.WorkspaceIdentity(req.Context())
		open, err := store.OpenPublic(req.Context(), req.URL.Query().Get("topic"))
		if err != nil {
			writeErr(w, err)
			return
		}
		joined, err := store.Joined(req.Context(), ws)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"rooms": open, "joined": joined})
	})
	r.Get("/v1/rooms/{roomID}", func(w http.ResponseWriter, req *http.Request) {
		ws, admin := auth.WorkspaceIdentity(req.Context())
		room, err := store.Get(req.Context(), ws, admin, chi.URLParam(req, "roomID"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, room)
	})
	r.Post("/v1/rooms/{roomID}/join", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, user, ok := actor(w, req)
		if !ok {
			return
		}
		var in struct {
			TermsVersion int `json:"terms_version"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {terms_version}: "+err.Error())
			return
		}
		m, created, err := store.Join(req.Context(), ws, user, chi.URLParam(req, "roomID"), in.TermsVersion)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, status(created), m)
	}))
	r.Put("/v1/rooms/{roomID}/terms", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		var in rooms.TermsDraft
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {split_rule, remix_share_bps, default_price_usd_micros, spend_policy}: "+err.Error())
			return
		}
		t, err := store.SetTerms(req.Context(), ws, chi.URLParam(req, "roomID"), in)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, t)
	}))
	r.Patch("/v1/rooms/{roomID}/members/{ws}", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		var in rooms.MemberChange
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {role, may_spend, remove}: "+err.Error())
			return
		}
		m, err := store.ChangeMember(req.Context(), ws, chi.URLParam(req, "roomID"), chi.URLParam(req, "ws"), in)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, m)
	}))
	r.Post("/v1/rooms/{roomID}/agents", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		var in struct {
			AgentID string `json:"agent_id"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {agent_id}: "+err.Error())
			return
		}
		a, created, err := store.AddAgent(req.Context(), ws, chi.URLParam(req, "roomID"), in.AgentID)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, status(created), a)
	}))
}

// roomActorOnly admits the workspace's owner or an admin — the rule publishing a listing uses — so an agent's own key
// never joins its workspace to a room or changes who is in one.
func roomActorOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if _, ok := storedanswers.OwnerOrAdmin(req.Context()); !ok {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin acts for it in a room")
			return
		}
		next(w, req)
	}
}
