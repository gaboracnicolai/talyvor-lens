package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B20.5 — SELLERS ARE PAID IN MONEY, THROUGH STRIPE CONNECT (internal/market/payout.go).
//
//	GET  /v1/workspaces/{wsID}/marketplace/payouts          the seller's account, balance, next payout with its fees, and payouts
//	POST /v1/workspaces/{wsID}/marketplace/payouts/connect  {country, email}  a link to Stripe's onboarding (creating the
//	                                                       account, with email — the signed-in person's — as its contact)
//	POST /v1/workspaces/{wsID}/marketplace/payouts/credits  take the available balance as Talyvor credits, 1:1
//	GET  /v1/workspaces/{wsID}/marketplace/statements       the weeks the seller was paid in (B32.42); ?period=2026-W41
//	                                                       that week's statement: sales, Talyvor's fee, royalties paid
//	                                                       and received, refunds, credits, Stripe's fees and the net;
//	                                                       the owner or an admin, as for the seller's tax details
//
// connectFor is the Connect client a workspace's seller account is made and paid with — a test workspace's in
// Stripe test mode (B25.6). nil (billing is off, or no test-mode key for a test workspace): the seller cannot
// connect, told why, and the page shows what was last recorded.

type marketPayoutURLs struct{ refresh, ret string }

// connectByWorkspace is a workspace's Connect client, or nil and why it has none: errPayoutsOff, or (B25.6)
// errNoTestMode.
type connectByWorkspace func(wsID string) (market.ConnectStripe, error)

var errPayoutsOff = errors.New("payouts are not configured here: Stripe billing is off")

func mountMarketPayoutRoutes(r chi.Router, store *market.Store, connectFor connectByWorkspace, crediter market.Crediter, urls marketPayoutURLs) {
	r.Get("/v1/workspaces/{wsID}/marketplace/payouts", func(w http.ResponseWriter, req *http.Request) {
		wsID := chi.URLParam(req, "wsID")
		connect, _ := connectFor(wsID)
		p, err := store.SellerPayouts(req.Context(), connect, wsID, time.Now())
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, p)
	})
	r.Post("/v1/workspaces/{wsID}/marketplace/payouts/connect", marketOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		connect, err := connectFor(chi.URLParam(req, "wsID"))
		if connect == nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, errNoTestMode) {
				status = http.StatusForbidden
			}
			writeJSONErr(w, status, err.Error())
			return
		}
		var in struct {
			Country string `json:"country"`
			Email   string `json:"email"`
		}
		if req.ContentLength != 0 {
			if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, `body must be {"country", "email"}: `+err.Error())
				return
			}
		}
		url, account, err := store.ConnectSeller(req.Context(), connect, chi.URLParam(req, "wsID"), in.Country, in.Email, urls.refresh, urls.ret)
		if err != nil && !errors.Is(err, market.ErrInvalid) {
			// Stripe's code and request id, which the seller's sentence leaves out (B28.276).
			slog.Warn("market: connect with Stripe", "workspace", chi.URLParam(req, "wsID"), "err", err)
		}
		switch {
		case errors.Is(err, market.ErrInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case stripeRefusal(err) != "": // B26.12: why Stripe will not open the seller's account
			writeJSONErr(w, http.StatusBadRequest, stripeRefusal(err))
		case err != nil:
			writeJSONErr(w, http.StatusBadGateway, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, map[string]any{"url": url, "account": account})
		}
	}))
	r.Post("/v1/workspaces/{wsID}/marketplace/payouts/credits", marketOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		p, err := store.TakeAsCredits(req.Context(), crediter, chi.URLParam(req, "wsID"), time.Now())
		switch {
		case errors.Is(err, market.ErrNothingAvailable), errors.Is(err, market.ErrTaxHold):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusCreated, p)
		}
	}))
	r.Get("/v1/workspaces/{wsID}/marketplace/statements", func(w http.ResponseWriter, req *http.Request) {
		if _, ok := storedanswers.OwnerOrAdmin(req.Context()); !ok {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may read its statements")
			return
		}
		wsID, period := chi.URLParam(req, "wsID"), req.URL.Query().Get("period")
		if period == "" {
			list, err := store.SellerStatements(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]any{"statements": list})
			return
		}
		st, err := store.SellerStatement(req.Context(), wsID, period)
		switch {
		case errors.Is(err, market.ErrInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, st)
		}
	})
}

// payMarketSellers is the weekly payout run (B32.42), asked every hour: on payday (LENS_MARKET_PAYOUT_WEEKDAY, UTC)
// a seller is paid once their available balance has reached the minimum, at most once a week; every hour a transfer
// Stripe did not accept is retried. A test seller is paid in Stripe test mode (B25.6).
func payMarketSellers(ctx context.Context, store *market.Store, kinds stripeByKind, payday time.Weekday) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now()
			if n, err := kinds.payOut(ctx, store, now, now.UTC().Weekday() == payday); err != nil {
				slog.Warn("market: paying sellers", "paid", n, "err", err)
			} else if n > 0 {
				slog.Info("market: paid sellers", "paid", n)
			}
		}
	}
}
