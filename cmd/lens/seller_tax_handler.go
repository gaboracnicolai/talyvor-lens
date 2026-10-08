package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/sellertax"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B32.41 — a seller's tax details, as the UK reporting rules and DAC7 have a platform collect them.
//
//	GET /v1/workspaces/{wsID}/marketplace/seller-tax   the details — TINs and the account identifier masked to their
//	                                                   last four characters, the date of birth masked — what is still
//	                                                   missing, the requests sent, the next one, and any payout hold
//	PUT /v1/workspaces/{wsID}/marketplace/seller-tax   {seller_type, first_name, middle_name, last_name, legal_name,
//	                                                   address, country, tins: [{jurisdiction, number}], date_of_birth,
//	                                                   company_registration_number, vat_number, account_identifier,
//	                                                   account_holder, self_billing_agreed_version}
//
// tins, date_of_birth and account_identifier left out keep what is stored. A VAT number is checked with the tax
// partner when it is saved. Both take the workspace's owner or an admin: they are a person's identity and bank
// details. Without LENS_PROVIDER_SECRET_KEK nothing is stored (503), and accepting reads false.
type sellerTaxDetails interface {
	Get(ctx context.Context, workspaceID string) (sellertax.Details, error)
	Put(ctx context.Context, workspaceID string, in sellertax.Input) (sellertax.Details, error)
}

func mountSellerTaxRoutes(r chi.Router, store sellerTaxDetails) {
	ownerOnly := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			if _, ok := storedanswers.OwnerOrAdmin(req.Context()); !ok {
				writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may read or change its tax details")
				return
			}
			next(w, req)
		}
	}
	r.Get("/v1/workspaces/{wsID}/marketplace/seller-tax", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		d, err := store.Get(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, d)
	}))
	r.Put("/v1/workspaces/{wsID}/marketplace/seller-tax", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in sellertax.Input
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be the seller's tax details as JSON: "+err.Error())
			return
		}
		d, err := store.Put(req.Context(), chi.URLParam(req, "wsID"), in)
		switch {
		case errors.Is(err, sellertax.ErrInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, sellertax.ErrNoCustody):
			writeJSONErr(w, http.StatusServiceUnavailable, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, d)
		}
	}))
}

// remindSellersOfTaxDetails asks, every hour, each seller with earnings and incomplete tax details for them — at
// their first earning, then twice more — and starts the payout hold the last request leads to (B32.41).
func remindSellersOfTaxDetails(ctx context.Context, store *sellertax.Store) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if sent, err := store.Remind(ctx); err != nil {
			slog.Warn("sellertax: reminding sellers", "err", err)
		} else if len(sent) > 0 {
			slog.Info("sellertax: asked sellers for their tax details", "sellers", len(sent))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
