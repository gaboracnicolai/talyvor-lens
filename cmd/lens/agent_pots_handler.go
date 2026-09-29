package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B22.7 — pots: an agent keeps money aside for a goal (economy/agent_pots.go).
//
//	GET  /v1/workspaces/{wsID}/agents/{agentID}/pots                the agent's pots and what each holds
//	POST /v1/workspaces/{wsID}/agents/{agentID}/pots                {name, kind, target_ulxc, locked_until}
//	POST /v1/workspaces/{wsID}/agents/{agentID}/pots/{potID}/in     {amount_ulxc} — from the agent's balance
//	POST /v1/workspaces/{wsID}/agents/{agentID}/pots/{potID}/out    {amount_ulxc} — back, unless locked
//	PUT  /v1/workspaces/{wsID}/agents/{agentID}/pots/{potID}/lock   {locked_until} — RFC 3339, or null to unlock
//
// Each takes the agent's own key, the workspace's owner or an admin. Mounted in the authed group.

type agentPotBank interface {
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
	ListPots(ctx context.Context, workspaceID, agentID string) ([]economy.Pot, error)
	CreatePot(ctx context.Context, workspaceID, agentID, name, kind string, target int64, lockedUntil *time.Time) (economy.Pot, error)
	MoveToPot(ctx context.Context, workspaceID, agentID, potID string, amount int64) (economy.Pot, error)
	MoveFromPot(ctx context.Context, workspaceID, agentID, potID string, amount int64) (economy.Pot, error)
	LockPot(ctx context.Context, workspaceID, agentID, potID string, until *time.Time) (economy.Pot, error)
}

func mountAgentPotRoutes(r chi.Router, bank agentPotBank) {
	// agentOrOwner lets the agent's own key, the workspace's owner or an admin manage the agent's pots.
	agentOrOwner := func(next func(w http.ResponseWriter, req *http.Request, ws, agentID string)) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			agentID := chi.URLParam(req, "agentID")
			if _, owner := storedanswers.OwnerOrAdmin(req.Context()); !owner {
				actx := auth.GetAuthContext(req.Context())
				keyAgent := ""
				if actx != nil && actx.APIKeyID != "" {
					keyAgent, _, _ = bank.AgentOfKey(req.Context(), actx.APIKeyID)
				}
				if keyAgent != agentID {
					writeJSONErr(w, http.StatusForbidden, "only the agent's own key, the workspace's owner or an admin may manage its pots")
					return
				}
			}
			next(w, req, chi.URLParam(req, "wsID"), agentID)
		}
	}
	r.Get("/v1/workspaces/{wsID}/agents/{agentID}/pots", agentOrOwner(func(w http.ResponseWriter, req *http.Request, ws, agentID string) {
		pots, err := bank.ListPots(req.Context(), ws, agentID)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"pots": pots})
	}))
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/pots", agentOrOwner(func(w http.ResponseWriter, req *http.Request, ws, agentID string) {
		var in struct {
			Name        string     `json:"name"`
			Kind        string     `json:"kind"`
			TargetULXC  int64      `json:"target_ulxc"`
			LockedUntil *time.Time `json:"locked_until"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"name": "…", "kind": "goal|budget|reserve", "target_ulxc": <µLXC, optional>, "locked_until": "<RFC 3339, optional>"}`)
			return
		}
		p, err := bank.CreatePot(req.Context(), ws, agentID, in.Name, in.Kind, in.TargetULXC, in.LockedUntil)
		if err != nil {
			writePotErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, p)
	}))
	for _, dir := range []string{"in", "out"} {
		move := bank.MoveToPot
		if dir == "out" {
			move = bank.MoveFromPot
		}
		r.Post("/v1/workspaces/{wsID}/agents/{agentID}/pots/{potID}/"+dir, agentOrOwner(func(w http.ResponseWriter, req *http.Request, ws, agentID string) {
			var in struct {
				AmountULXC int64 `json:"amount_ulxc"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.AmountULXC <= 0 {
				writeJSONErr(w, http.StatusBadRequest, `body must be {"amount_ulxc": <positive µLXC>}`)
				return
			}
			p, err := move(req.Context(), ws, agentID, chi.URLParam(req, "potID"), in.AmountULXC)
			if err != nil {
				writePotErr(w, err)
				return
			}
			writeJSONOK(w, http.StatusOK, p)
		}))
	}
	r.Put("/v1/workspaces/{wsID}/agents/{agentID}/pots/{potID}/lock", agentOrOwner(func(w http.ResponseWriter, req *http.Request, ws, agentID string) {
		var in struct {
			LockedUntil *time.Time `json:"locked_until"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"locked_until": "<RFC 3339>" | null}`)
			return
		}
		p, err := bank.LockPot(req.Context(), ws, agentID, chi.URLParam(req, "potID"), in.LockedUntil)
		if err != nil {
			writePotErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, p)
	}))
}

func writePotErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, economy.ErrPotNotFound), errors.Is(err, economy.ErrAgentNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, economy.ErrPot):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, economy.ErrPotLocked), errors.Is(err, economy.ErrAgentFunds):
		writeJSONErr(w, http.StatusConflict, err.Error())
	default:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
	}
}
