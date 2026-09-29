package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B22.8 — investing and trading, simulated until a broker partner exists (economy/sim_trading.go).
//
//	GET  /v1/markets/simulated/quotes                                          every instrument and its price, from the ECB
//	GET  /v1/workspaces/{wsID}/agents/{agentID}/portfolios                    the agent's portfolios, valued at the quotes
//	POST /v1/workspaces/{wsID}/agents/{agentID}/portfolios                    {name, cash_uusd} — simulated US dollars
//	GET  /v1/workspaces/{wsID}/agents/{agentID}/portfolios/{pfID}
//	POST /v1/workspaces/{wsID}/agents/{agentID}/portfolios/{pfID}/orders      {instrument, side, type, quantity_micros,
//	                                                                          limit_price_usd}
//	POST /v1/workspaces/{wsID}/agents/{agentID}/portfolios/{pfID}/orders/{orderID}/cancel
//
// Everything is simulated: no order is sent to any market and no credits move. Each route but the quotes takes
// the agent's own key, the workspace's owner or an admin. Open limit orders fill on the agent schedules' tick.
// Mounted in the authed group.

type simTradingBank interface {
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
	SimQuotes(ctx context.Context) (economy.Quotes, error)
	OpenPortfolio(ctx context.Context, workspaceID, agentID, name string, cashUUSD int64) (economy.Portfolio, error)
	GetPortfolio(ctx context.Context, workspaceID, agentID, portfolioID string) (economy.Portfolio, error)
	ListPortfolios(ctx context.Context, workspaceID, agentID string) ([]economy.Portfolio, error)
	PlaceSimOrder(ctx context.Context, workspaceID, agentID, portfolioID string, in economy.SimOrderInput) (economy.SimOrder, error)
	CancelSimOrder(ctx context.Context, workspaceID, agentID, portfolioID, orderID string) (economy.SimOrder, error)
}

func mountSimTradingRoutes(r chi.Router, bank simTradingBank) {
	// agentOrOwner lets the agent's own key, the workspace's owner or an admin trade in the agent's portfolios.
	agentOrOwner := func(next func(w http.ResponseWriter, req *http.Request, ws, agentID string)) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			ws, agentID := chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID")
			if _, owner := storedanswers.OwnerOrAdmin(req.Context()); !owner {
				actx := auth.GetAuthContext(req.Context())
				if actx == nil || actx.APIKeyID == "" {
					writeJSONErr(w, http.StatusForbidden, "only the agent's own key, the workspace's owner or an admin may trade in its portfolios")
					return
				}
				if keyAgent, _, _ := bank.AgentOfKey(req.Context(), actx.APIKeyID); keyAgent != agentID {
					writeJSONErr(w, http.StatusForbidden, "only the agent's own key, the workspace's owner or an admin may trade in its portfolios")
					return
				}
			}
			next(w, req, ws, agentID)
		}
	}
	r.Get("/v1/markets/simulated/quotes", func(w http.ResponseWriter, req *http.Request) {
		q, err := bank.SimQuotes(req.Context())
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, q)
	})
	r.Get("/v1/workspaces/{wsID}/agents/{agentID}/portfolios", agentOrOwner(func(w http.ResponseWriter, req *http.Request, ws, agentID string) {
		list, err := bank.ListPortfolios(req.Context(), ws, agentID)
		if err != nil {
			writeSimErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"portfolios": list, "notice": economy.SimulatedNotice})
	}))
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/portfolios", agentOrOwner(func(w http.ResponseWriter, req *http.Request, ws, agentID string) {
		var in struct {
			Name     string `json:"name"`
			CashUUSD int64  `json:"cash_uusd"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"name": "…", "cash_uusd": <simulated µUSD>}`)
			return
		}
		p, err := bank.OpenPortfolio(req.Context(), ws, agentID, in.Name, in.CashUUSD)
		if err != nil {
			writeSimErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, p)
	}))
	r.Get("/v1/workspaces/{wsID}/agents/{agentID}/portfolios/{pfID}", agentOrOwner(func(w http.ResponseWriter, req *http.Request, ws, agentID string) {
		p, err := bank.GetPortfolio(req.Context(), ws, agentID, chi.URLParam(req, "pfID"))
		if err != nil {
			writeSimErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, p)
	}))
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/portfolios/{pfID}/orders", agentOrOwner(func(w http.ResponseWriter, req *http.Request, ws, agentID string) {
		var in economy.SimOrderInput
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"instrument": "EUR", "side": "buy|sell", "type": "market|limit", "quantity_micros": …, "limit_price_usd": "<for a limit order>"}`)
			return
		}
		o, err := bank.PlaceSimOrder(req.Context(), ws, agentID, chi.URLParam(req, "pfID"), in)
		if err != nil {
			writeSimErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, o)
	}))
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/portfolios/{pfID}/orders/{orderID}/cancel", agentOrOwner(func(w http.ResponseWriter, req *http.Request, ws, agentID string) {
		o, err := bank.CancelSimOrder(req.Context(), ws, agentID, chi.URLParam(req, "pfID"), chi.URLParam(req, "orderID"))
		if err != nil {
			writeSimErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, o)
	}))
}

func writeSimErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, economy.ErrPortfolioNotFound), errors.Is(err, economy.ErrSimOrderNotFound), errors.Is(err, economy.ErrAgentNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, economy.ErrSimOrder), errors.Is(err, economy.ErrNoQuote):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, economy.ErrSimHoldings):
		writeJSONErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, economy.ErrLiveTrading):
		writeJSONErr(w, http.StatusForbidden, err.Error())
	default:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
	}
}
