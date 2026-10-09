package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B30.9 — terms for each capability, accepted before first use (economy/capability_terms.go). A capability with terms
// refuses use, test money or live, until a person of the workspace accepts their latest version; a new version asks
// again. The texts are docs/terms/<capability>.md, headed "Draft — for legal review".
//
//	GET  /v1/workspaces/{wsID}/terms                            every capability with terms: its latest version and
//	                                                            whether the workspace has accepted it
//	GET  /v1/workspaces/{wsID}/terms/{capability}               the latest version's text, and whether it is accepted
//	POST /v1/workspaces/{wsID}/terms/{capability}/accept        {"version": n} — accept version n, which must be the
//	                                                            latest (409 otherwise); 201 with the acceptance
//
// Any credential of the workspace reads them. Accepting takes the workspace's owner — a person of the workspace, never
// the operator — and records who, when and an HMAC of the address it came from.
type capabilityTermsStore interface {
	WorkspaceTermsList(ctx context.Context, workspaceID string) ([]economy.WorkspaceTerms, error)
	WorkspaceTermsFor(ctx context.Context, workspaceID, capability string) (economy.WorkspaceTerms, error)
	AcceptTerms(ctx context.Context, workspaceID, capability string, version int, person, ip string) (economy.TermsAcceptance, error)
}

func mountCapabilityTermsRoutes(r chi.Router, store capabilityTermsStore) {
	r.Get("/v1/workspaces/{wsID}/terms", func(w http.ResponseWriter, req *http.Request) {
		ts, err := store.WorkspaceTermsList(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"terms": ts})
	})
	r.Get("/v1/workspaces/{wsID}/terms/{capability}", func(w http.ResponseWriter, req *http.Request) {
		t, err := store.WorkspaceTermsFor(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "capability"))
		switch {
		case errors.Is(err, economy.ErrNoTerms):
			writeJSONErr(w, http.StatusNotFound, err.Error())
			return
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, t)
	})
	r.Post("/v1/workspaces/{wsID}/terms/{capability}/accept", func(w http.ResponseWriter, req *http.Request) {
		who, ok := storedanswers.OwnerOrAdmin(req.Context())
		switch {
		case !ok:
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner may accept its terms")
			return
		case who == "operator":
			writeJSONErr(w, http.StatusForbidden, "terms are accepted by a person of the workspace, not by the operator")
			return
		}
		var in struct {
			Version int `json:"version"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil || in.Version <= 0 {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"version": n}, the version of the terms read`)
			return
		}
		a, err := store.AcceptTerms(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "capability"), in.Version, who, clientIP(req))
		switch {
		case errors.Is(err, economy.ErrNoTerms):
			writeJSONErr(w, http.StatusNotFound, err.Error())
			return
		case errors.Is(err, economy.ErrTermsVersionStale):
			writeJSONErr(w, http.StatusConflict, err.Error())
			return
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusCreated, map[string]any{"acceptance": a})
	})
}
