package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B20.1 — PUBLISH: a listing is a versioned agent, prompt, skill, evaluation or pipeline (internal/market).
//
//	POST /v1/workspaces/{wsID}/marketplace/listings                {kind, title, description, price_per_use_ulxc, visibility, artifact, changelog}
//	POST /v1/workspaces/{wsID}/marketplace/listings/{id}/versions  {artifact, changelog}   a new version; the old ones stay usable
//	GET  /v1/workspaces/{wsID}/marketplace/listings                the workspace's own listings
//	GET  /v1/marketplace/listings?kind=                            the public catalog
//	GET  /v1/marketplace/listings/{id}                             a listing and its versions (artifacts for its owner only)
//	POST /v1/marketplace/listings/{id}/reports                     {reason, details}   B20.4: report a listing
//
// A publish the scan refuses is 422 with what it found (secrets, personal data, prompt injection); one the
// review holds (B20.4, internal/market/review.go) is 201 with review_status "held" and the reason, and only
// its owner sees it until an admin approves it. Publishing takes the workspace's owner or an admin; reading
// and reporting take any of its credentials.

func mountMarketRoutes(r chi.Router, store *market.Store) {
	writeErr := func(w http.ResponseWriter, err error) {
		var refused *market.RefusedError
		switch {
		case errors.As(err, &refused):
			writeJSONOK(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error(), "scan": refused.Scan})
		case errors.Is(err, market.ErrInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, market.ErrNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, market.ErrTakenDown):
			writeJSONErr(w, http.StatusGone, err.Error())
		default:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		}
	}
	r.Post("/v1/workspaces/{wsID}/marketplace/listings", marketOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		var d market.Draft
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, market.MaxArtifactBytes+64<<10)).Decode(&d); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be the listing: "+err.Error())
			return
		}
		l, err := store.Publish(req.Context(), chi.URLParam(req, "wsID"), d)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, l)
	}))
	r.Post("/v1/workspaces/{wsID}/marketplace/listings/{listingID}/versions", marketOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Artifact  json.RawMessage `json:"artifact"`
			Changelog string          `json:"changelog"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, market.MaxArtifactBytes+64<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {artifact, changelog}: "+err.Error())
			return
		}
		v, err := store.PublishVersion(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "listingID"), in.Artifact, in.Changelog)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, v)
	}))
	r.Get("/v1/workspaces/{wsID}/marketplace/listings", func(w http.ResponseWriter, req *http.Request) {
		list, err := store.OwnListings(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"listings": list})
	})
	r.Get("/v1/marketplace/listings", func(w http.ResponseWriter, req *http.Request) {
		list, err := store.Catalog(req.Context(), req.URL.Query().Get("kind"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"listings": list})
	})
	r.Get("/v1/marketplace/listings/{listingID}", func(w http.ResponseWriter, req *http.Request) {
		viewer, _ := auth.WorkspaceIdentity(req.Context())
		l, err := store.Get(req.Context(), viewer, chi.URLParam(req, "listingID"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, l)
	})
	r.Post("/v1/marketplace/listings/{listingID}/reports", func(w http.ResponseWriter, req *http.Request) {
		reporter, _ := auth.WorkspaceIdentity(req.Context())
		if reporter == "" {
			writeJSONErr(w, http.StatusForbidden, "a report needs a workspace's credential")
			return
		}
		var in struct {
			Reason  string `json:"reason"`
			Details string `json:"details"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {reason, details}: "+err.Error())
			return
		}
		rep, err := store.Report(req.Context(), reporter, chi.URLParam(req, "listingID"), in.Reason, in.Details)
		if err != nil {
			writeErr(w, err)
			return
		}
		status := http.StatusCreated
		if rep.AlreadyMade {
			status = http.StatusOK
		}
		writeJSONOK(w, status, rep)
	})
}

// B20.4 — the admin's half of marketplace safety (requireAdmin):
//
//	GET  /v1/admin/marketplace/review                        held listings and reported ones, most reported first
//	POST /v1/admin/marketplace/listings/{id}/approve         keep it up: release a hold, resolve its reports as kept
//	POST /v1/admin/marketplace/listings/{id}/takedown        {reason}   take it down and refund its uses inside the holdback
//
// A takedown answers with the refunds it wrote (market_refunds rows); a buyer's credit Stripe did not
// accept is named in credit_error and retried by refundTakenDownMarketUses.
func mountMarketAdminRoutes(r chi.Router, am adminAuthenticator, store *market.Store, refunder market.Refunder) {
	writeErr := func(w http.ResponseWriter, err error) {
		switch {
		case errors.Is(err, market.ErrInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, market.ErrNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, market.ErrTakenDown):
			writeJSONErr(w, http.StatusConflict, err.Error())
		default:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		}
	}
	r.Get("/v1/admin/marketplace/review", requireAdmin(am, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		q, err := store.ReviewQueue(req.Context())
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"listings": q})
	})))
	r.Post("/v1/admin/marketplace/listings/{listingID}/approve", requireAdmin(am, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		l, err := store.Approve(req.Context(), chi.URLParam(req, "listingID"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, l)
	})))
	r.Post("/v1/admin/marketplace/listings/{listingID}/takedown", requireAdmin(am, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Reason string `json:"reason"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {reason}: "+err.Error())
			return
		}
		t, err := store.TakeDown(req.Context(), refunder, chi.URLParam(req, "listingID"), in.Reason)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, t)
	})))
}

// marketOwnerOnly admits the workspace's owner or an admin — the rule the agent routes use.
func marketOwnerOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if _, ok := storedanswers.OwnerOrAdmin(req.Context()); !ok {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may publish its listings")
			return
		}
		next(w, req)
	}
}
