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
// B19.2 — each agent's spending rules, and the approvals its rules ask a person for:
//
//	GET  /v1/workspaces/{wsID}/agents/{id}/rules              the agent's rules
//	PUT  /v1/workspaces/{wsID}/agents/{id}/rules   {rules}    replace them (economy.AgentRules)
//	GET  /v1/workspaces/{wsID}/agents/approvals                requests that needed approval, newest first
//	POST /v1/workspaces/{wsID}/agents/approvals/{id}/approve   let that request through, once
//	POST /v1/workspaces/{wsID}/agents/approvals/{id}/deny      refuse it
//
// B19.3 — one company's agents pay each other, inside the closed loop:
//
//	POST /v1/workspaces/{wsID}/agents/{id}/pay  {"to_agent_id", "amount_ulxc", "memo"}   by the paying agent's own key, or the owner
//
// Mounted in the authed group, so {wsID} is bound to the caller's credential. Moving money, creating
// agents and issuing keys take the workspace's owner or an admin; reading takes any of its credentials.

type agentBank interface {
	CreateAgent(ctx context.Context, workspaceID, name string) (economy.Agent, error)
	AttachAgentKey(ctx context.Context, workspaceID, agentID, scopedKeyID string) error
	FundAgent(ctx context.Context, workspaceID, agentID string, amount int64) (int64, error)
	WithdrawAgent(ctx context.Context, workspaceID, agentID string, amount int64) (int64, error)
	AgentBook(ctx context.Context, workspaceID string) (economy.AgentBook, error)
	SetAgentRules(ctx context.Context, workspaceID, agentID string, r economy.AgentRules) (economy.AgentRules, error)
	GetAgentRules(ctx context.Context, workspaceID, agentID string) (economy.AgentRules, error)
	ListAgentApprovals(ctx context.Context, workspaceID string) ([]economy.AgentApproval, error)
	DecideAgentApproval(ctx context.Context, workspaceID, approvalID string, approve bool) (economy.AgentApproval, error)
	PayAgent(ctx context.Context, workspaceID, fromAgentID, toAgentID string, amount int64, memo string) (economy.AgentPayment, error)
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
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

	writeRules := func(w http.ResponseWriter, rules economy.AgentRules, err error) {
		switch {
		case errors.Is(err, economy.ErrAgentNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrAgentRule):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, rules)
		}
	}
	r.Get("/v1/workspaces/{wsID}/agents/{agentID}/rules", func(w http.ResponseWriter, req *http.Request) {
		rules, err := bank.GetAgentRules(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"))
		writeRules(w, rules, err)
	})
	r.Put("/v1/workspaces/{wsID}/agents/{agentID}/rules", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in economy.AgentRules
		dec := json.NewDecoder(req.Body)
		dec.DisallowUnknownFields() // a misspelt rule must not be silently no rule
		if err := dec.Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be the agent's rules: "+err.Error())
			return
		}
		rules, err := bank.SetAgentRules(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"), in)
		writeRules(w, rules, err)
	}))
	r.Get("/v1/workspaces/{wsID}/agents/approvals", func(w http.ResponseWriter, req *http.Request) {
		list, err := bank.ListAgentApprovals(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"approvals": list})
	})
	decide := func(approve bool) http.HandlerFunc {
		return ownerOnly(func(w http.ResponseWriter, req *http.Request) {
			a, err := bank.DecideAgentApproval(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "approvalID"), approve)
			switch {
			case errors.Is(err, economy.ErrApprovalNotFound):
				writeJSONErr(w, http.StatusNotFound, err.Error())
			case err != nil:
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
			default:
				writeJSONOK(w, http.StatusOK, a)
			}
		})
	}
	r.Post("/v1/workspaces/{wsID}/agents/approvals/{approvalID}/approve", decide(true))
	r.Post("/v1/workspaces/{wsID}/agents/approvals/{approvalID}/deny", decide(false))

	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/pay", func(w http.ResponseWriter, req *http.Request) {
		wsID, agentID := chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID")
		if _, owner := storedanswers.OwnerOrAdmin(req.Context()); !owner {
			keyAgent := ""
			if actx := auth.GetAuthContext(req.Context()); actx != nil && actx.APIKeyID != "" {
				keyAgent, _, _ = bank.AgentOfKey(req.Context(), actx.APIKeyID)
			}
			if keyAgent != agentID {
				writeJSONErr(w, http.StatusForbidden, "only the paying agent's own key, the workspace's owner or an admin may pay from an agent")
				return
			}
		}
		var in struct {
			ToAgentID  string `json:"to_agent_id"`
			AmountULXC int64  `json:"amount_ulxc"`
			Memo       string `json:"memo"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.ToAgentID == "" || in.AmountULXC <= 0 {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"to_agent_id": "<agent>", "amount_ulxc": <positive µLXC>, "memo": "<optional>"}`)
			return
		}
		pay, err := bank.PayAgent(req.Context(), wsID, agentID, in.ToAgentID, in.AmountULXC, in.Memo)
		switch {
		case errors.Is(err, economy.ErrAgentNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrSameAgent):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, economy.ErrAgentFunds):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, economy.ErrAgentRule), errors.Is(err, economy.ErrApprovalRequired):
			writeJSONErr(w, http.StatusForbidden, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, pay)
		}
	})
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
