package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/rooms"
)

// B32.35 — room prizes (internal/rooms/prizes.go):
//
//	POST /v1/rooms/{roomID}/prizes                      {title, criteria, amount_usd_micros, deadline}   the room's owner
//	                                                    posts a prize; 201 with the prize and the message the room got
//	GET  /v1/rooms/{roomID}/prizes                      the room's prizes, newest first, as its readers see them
//	POST /v1/rooms/{roomID}/prizes/{prizeID}/award      {contribution_id}   the room's owner awards it; 200 with the
//	                                                    prize and the owner's licence to what won
//
// A prize larger than what the room wallet's monthly limit has left this month, less the room's other open prizes, is
// 403 and writes nothing. Awarding buys the winning contribution at the prize's amount: one billed prize row on the
// owner's marketplace bill, judged by the room wallet's rules — a refusal is 403 naming the rule, one above its approval
// amount 403 with the approval the owner is asked for — and a perpetual commercial licence for the owner. A prize
// already awarded, or past its deadline, is 409: past its deadline it closes and nothing is charged.
func mountRoomPrizeRoutes(r chi.Router, store *rooms.Store, meter market.Meter, agents marketAgents) {
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
	writeErr := func(w http.ResponseWriter, err error) {
		if errors.Is(err, rooms.ErrOverBudget) {
			writeJSONErr(w, http.StatusForbidden, err.Error())
			return
		}
		writeRoomRunErr(w, err)
	}

	r.Post("/v1/rooms/{roomID}/prizes", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, user, ok := actor(w, req)
		if !ok {
			return
		}
		var in rooms.PrizeDraft
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 64<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"title", "criteria", "amount_usd_micros", "deadline"}: `+err.Error())
			return
		}
		p, err := store.PostPrize(req.Context(), ws, user, chi.URLParam(req, "roomID"), in)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, p)
	}))
	r.Get("/v1/rooms/{roomID}/prizes", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := actor(w, req)
		if !ok {
			return
		}
		ps, err := store.Prizes(req.Context(), ws, chi.URLParam(req, "roomID"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"prizes": ps})
	})
	r.Post("/v1/rooms/{roomID}/prizes/{prizeID}/award", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, user, ok := actor(w, req)
		if !ok {
			return
		}
		var in struct {
			ContributionID string `json:"contribution_id"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"contribution_id"}: `+err.Error())
			return
		}
		m := meter
		if k, ok := meter.(stripeByKind); ok {
			m = k.meterFor(ws) // B25.6: a test workspace's purchases go on its Stripe test-mode bill
		}
		deps := market.LicenceDeps{Meter: m, Agents: agents, Capabilities: agents}
		a, err := store.AwardPrize(req.Context(), deps, ws, user, chi.URLParam(req, "roomID"), chi.URLParam(req, "prizeID"), in.ContributionID)
		if err != nil {
			writeErr(w, err)
			return
		}
		if a.Licence.MeterError != "" {
			slog.Warn("market: a room's prize was awarded but is not yet on the owner's bill; the next pass bills it",
				"use", a.Licence.UseID, "workspace", ws, "err", a.Licence.MeterError)
		}
		writeJSONOK(w, http.StatusOK, a)
	}))
}
