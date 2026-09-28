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
)

// B20.5 — SELLERS ARE PAID IN MONEY, THROUGH STRIPE CONNECT (internal/market/payout.go).
//
//	GET  /v1/workspaces/{wsID}/marketplace/payouts          the seller's account, balance, next payout with its fees, and payouts
//	POST /v1/workspaces/{wsID}/marketplace/payouts/connect  {country}  a link to Stripe's onboarding (creating the account)
//	POST /v1/workspaces/{wsID}/marketplace/payouts/credits  take the available balance as Talyvor credits, 1:1
//
// connect nil (billing is off): the seller cannot connect, and the page shows what was last recorded.

type marketPayoutURLs struct{ refresh, ret string }

func mountMarketPayoutRoutes(r chi.Router, store *market.Store, connect market.ConnectStripe, crediter market.Crediter, urls marketPayoutURLs) {
	r.Get("/v1/workspaces/{wsID}/marketplace/payouts", func(w http.ResponseWriter, req *http.Request) {
		p, err := store.SellerPayouts(req.Context(), connect, chi.URLParam(req, "wsID"), time.Now())
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, p)
	})
	r.Post("/v1/workspaces/{wsID}/marketplace/payouts/connect", marketOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		if connect == nil {
			writeJSONErr(w, http.StatusServiceUnavailable, "payouts are not configured here: Stripe billing is off")
			return
		}
		var in struct {
			Country string `json:"country"`
		}
		if req.ContentLength != 0 {
			if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, `body must be {"country"}: `+err.Error())
				return
			}
		}
		url, account, err := store.ConnectSeller(req.Context(), connect, chi.URLParam(req, "wsID"), in.Country, urls.refresh, urls.ret)
		switch {
		case errors.Is(err, market.ErrInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusBadGateway, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, map[string]any{"url": url, "account": account})
		}
	}))
	r.Post("/v1/workspaces/{wsID}/marketplace/payouts/credits", marketOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		p, err := store.TakeAsCredits(req.Context(), crediter, chi.URLParam(req, "wsID"), time.Now())
		switch {
		case errors.Is(err, market.ErrNothingAvailable):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusCreated, p)
		}
	}))
}

// payMarketSellers is the monthly payout run, asked every hour: a seller is paid at most once a month, as
// soon as their available balance reaches the minimum, and a transfer Stripe did not accept is retried.
func payMarketSellers(ctx context.Context, store *market.Store, connect market.ConnectStripe) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := store.PayOut(ctx, connect, time.Now()); err != nil {
				slog.Warn("market: paying sellers", "paid", n, "err", err)
			} else if n > 0 {
				slog.Info("market: paid sellers", "paid", n)
			}
		}
	}
}
