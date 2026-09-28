package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
	"github.com/talyvor/lens/internal/tenant"
)

// B19.1 — AGENT ACCOUNTS: every agent has its own balance (internal/economy/agent_accounts.go).
//
//	GET  /v1/workspaces/{wsID}/agents                        the agents, their balances, reconciled with the workspace
//	POST /v1/workspaces/{wsID}/agents             {"name"}   create an agent
//	POST /v1/workspaces/{wsID}/agents/{id}/keys   {"name"}   issue the agent a proxy key of its own
//	POST /v1/workspaces/{wsID}/agents/{id}/fund     {"amount_ulxc"}   move the workspace's LXC to the agent
//	POST /v1/workspaces/{wsID}/agents/{id}/withdraw {"amount_ulxc"}   take it back
//
// Mounted in the authed group, so {wsID} is bound to the caller's credential. Moving money, creating
// agents and issuing keys take the workspace's owner or an admin; reading takes any of its credentials.

type agentBank interface {
	CreateAgent(ctx context.Context, workspaceID, name string) (economy.Agent, error)
	AttachAgentKey(ctx context.Context, workspaceID, agentID, scopedKeyID string) error
	FundAgent(ctx context.Context, workspaceID, agentID string, amount int64) (int64, error)
	WithdrawAgent(ctx context.Context, workspaceID, agentID string, amount int64) (int64, error)
	AgentBook(ctx context.Context, workspaceID string) (economy.AgentBook, error)
}

type agentKeyIssuer interface {
	CreateAPIKey(ctx context.Context, workspaceID, name string, scopes []string, expiresAt *time.Time) (string, *tenant.WorkspaceAPIKey, error)
}

func mountAgentAccountRoutes(r chi.Router, bank agentBank, keys agentKeyIssuer) {
	r.Get("/v1/workspaces/{wsID}/agents", func(w http.ResponseWriter, req *http.Request) {
		book, err := bank.AgentBook(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if book.Agents == nil {
			book.Agents = []economy.Agent{}
		}
		writeJSONOK(w, http.StatusOK, book)
	})
	r.Post("/v1/workspaces/{wsID}/agents", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.Name == "" {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"name": "<agent name>"}`)
			return
		}
		a, err := bank.CreateAgent(req.Context(), chi.URLParam(req, "wsID"), in.Name)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusCreated, a)
	}))
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/keys", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		wsID, agentID := chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID")
		var in struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(req.Body).Decode(&in)
		if in.Name == "" {
			in.Name = agentID
		}
		// The agent must exist here before a key is minted for it; AgentBook is the read that says so.
		book, err := bank.AgentBook(req.Context(), wsID)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		found := false
		for _, a := range book.Agents {
			found = found || a.ID == agentID
		}
		if !found {
			writeJSONErr(w, http.StatusNotFound, economy.ErrAgentNotFound.Error())
			return
		}
		raw, key, err := keys.CreateAPIKey(req.Context(), wsID, in.Name, []string{"proxy"}, nil)
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := bank.AttachAgentKey(req.Context(), wsID, agentID, key.ID); err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusCreated, map[string]any{
			"agent_id": agentID, "key": raw, "id": key.ID, "prefix": key.KeyPrefix,
			"warning": "Store this key securely. It will not be shown again.",
		})
	}))
	move := func(fn func(ctx context.Context, workspaceID, agentID string, amount int64) (int64, error)) http.HandlerFunc {
		return ownerOnly(func(w http.ResponseWriter, req *http.Request) {
			var in struct {
				AmountULXC int64 `json:"amount_ulxc"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.AmountULXC <= 0 {
				writeJSONErr(w, http.StatusBadRequest, `body must be {"amount_ulxc": <positive µLXC>}`)
				return
			}
			agentID := chi.URLParam(req, "agentID")
			bal, err := fn(req.Context(), chi.URLParam(req, "wsID"), agentID, in.AmountULXC)
			switch {
			case errors.Is(err, economy.ErrAgentNotFound):
				writeJSONErr(w, http.StatusNotFound, err.Error())
			case errors.Is(err, economy.ErrAgentFunds):
				writeJSONErr(w, http.StatusConflict, err.Error())
			case err != nil:
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
			default:
				writeJSONOK(w, http.StatusOK, map[string]any{"agent_id": agentID, "balance_ulxc": bal})
			}
		})
	}
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/fund", move(bank.FundAgent))
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/withdraw", move(bank.WithdrawAgent))
}

// ownerOnly admits the workspace's owner or an admin — the rule stored-answer deletion uses.
func ownerOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if _, ok := storedanswers.OwnerOrAdmin(req.Context()); !ok {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may manage its agents")
			return
		}
		next(w, req)
	}
}
