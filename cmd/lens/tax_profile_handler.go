package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/storedanswers"
	"github.com/talyvor/lens/internal/taxprofile"
)

// B32.38 — a buyer's tax profile and the evidence of where a buyer is.
//
//	GET /v1/workspaces/{wsID}/tax-profile   {profile, resolved}   the profile (null before one is saved) and where
//	                                                              the workspace resolves to, with its evidence
//	PUT /v1/workspaces/{wsID}/tax-profile   {legal_name, address, country, region, postal_code, business, tax_id}
//
// A tax id is checked with the tax partner when it is saved; one that is not valid is kept, marked invalid, and the
// workspace stays a consumer. resolved weighs the declared country against the billing country of the workspace's
// Stripe customer and the card country of its default payment method (taxprofile.Store.Resolve). Both take the
// workspace's owner or an admin: the profile is its legal name and address.
//
//	GET /v1/admin/tax-profiles/flagged      the profiles whose evidence contradicts the declaration, in main.go
type taxProfiles interface {
	Get(ctx context.Context, workspaceID string) (taxprofile.Profile, error)
	Put(ctx context.Context, workspaceID string, in taxprofile.Input) (taxprofile.Profile, error)
	Resolve(ctx context.Context, workspaceID string) (taxprofile.Resolution, error)
}

func mountTaxProfileRoutes(r chi.Router, store taxProfiles) {
	ownerOnly := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			if _, ok := storedanswers.OwnerOrAdmin(req.Context()); !ok {
				writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may read or change its tax profile")
				return
			}
			next(w, req)
		}
	}
	// answer is the profile and its resolution. A resolution Stripe could not be read for is null, with why: the
	// profile is still the workspace's to read and correct.
	answer := func(w http.ResponseWriter, req *http.Request, status int, profile *taxprofile.Profile) {
		body := map[string]any{"profile": profile, "resolved": nil}
		resolved, err := store.Resolve(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			body["resolve_error"] = err.Error()
		} else {
			body["resolved"] = resolved
		}
		writeJSONOK(w, status, body)
	}
	r.Get("/v1/workspaces/{wsID}/tax-profile", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		p, err := store.Get(req.Context(), chi.URLParam(req, "wsID"))
		switch {
		case errors.Is(err, taxprofile.ErrNotFound):
			answer(w, req, http.StatusOK, nil)
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			answer(w, req, http.StatusOK, &p)
		}
	}))
	r.Put("/v1/workspaces/{wsID}/tax-profile", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in taxprofile.Input
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 8<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {legal_name, address, country, region, postal_code, business, tax_id}: "+err.Error())
			return
		}
		p, err := store.Put(req.Context(), chi.URLParam(req, "wsID"), in)
		switch {
		case errors.Is(err, taxprofile.ErrInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusBadGateway, err.Error())
		default:
			answer(w, req, http.StatusOK, &p)
		}
	}))
}

// flaggedTaxProfiles is the slice of *taxprofile.Store the operator's read needs.
type flaggedTaxProfiles interface {
	Flagged(ctx context.Context, limit int) ([]taxprofile.Profile, error)
}

// newTaxProfilesFlaggedHandler answers GET /v1/admin/tax-profiles/flagged (B32.38): the buyer tax profiles whose
// Stripe evidence contradicts the declared country, the oldest flag first, each with its reason — for an operator
// to see to.
func newTaxProfilesFlaggedHandler(store flaggedTaxProfiles) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		list, err := store.Flagged(req.Context(), 500)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"profiles": list})
	})
}

// taxStripeByKind reads a workspace's Stripe evidence on the key its kind of workspace pays with: a test
// workspace's on the test-mode key (B25.2), the main key's while that is itself test mode, and none once the main
// key is live and there is no test-mode key.
type taxStripeByKind struct {
	isTest      func(string) bool
	mainKeyLive bool
	live, test  *billing.LiveStripe
}

func (k taxStripeByKind) CustomerCountries(ctx context.Context, workspaceID, customerID string) (string, string, error) {
	s := k.live
	if k.isTest(workspaceID) {
		switch {
		case k.test != nil:
			s = k.test
		case k.mainKeyLive:
			return "", "", nil
		}
	}
	if s == nil {
		return "", "", nil
	}
	return s.CustomerCountries(ctx, customerID)
}
