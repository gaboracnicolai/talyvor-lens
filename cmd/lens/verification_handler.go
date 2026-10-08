package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B30.4 — verification levels for people and companies (internal/economy/verification.go). Each check goes to the
// verification provider — the Test one until a real one is configured, and what it passes counts for test money
// only — and Lens keeps its level, method, status, date and the provider's reference, never a document.
//
//	GET  /v1/workspaces/{wsID}/verification            the level the checks reach, the level live money is judged by,
//	                                                   and every check, newest first; a pending check, or a pass, is
//	                                                   asked again first
//	POST /v1/workspaces/{wsID}/verification/contact    {email, phone}                          L1: email and phone confirmed
//	POST /v1/workspaces/{wsID}/verification/identity   {name, country, date_of_birth}          L2: identity checked
//	POST /v1/workspaces/{wsID}/verification/company    {name, country, company_number, directors, people_with_significant_control}
//	                                                                                           L3: company checked
//
// Each check needs the level below it (409 otherwise) and answers 201 with the check — passed, failed or pending —
// and the record. The level each capability needs for live money is level_needed on GET /v1/wallets/capabilities.
// All four take the workspace's owner or an admin: they are a person's identity.
type verificationStore interface {
	Verification(ctx context.Context, kyc partners.KYCProvider, workspaceID string) (economy.WorkspaceVerification, error)
	StartVerification(ctx context.Context, kyc partners.KYCProvider, workspaceID, by string, in economy.VerificationRequest) (economy.VerificationCheck, error)
}

func mountVerificationRoutes(r chi.Router, store verificationStore, verifier func() partners.KYCProvider) {
	r.Get("/v1/workspaces/{wsID}/verification", func(w http.ResponseWriter, req *http.Request) {
		if _, ok := storedanswers.OwnerOrAdmin(req.Context()); !ok {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may read its verification")
			return
		}
		v, err := store.Verification(req.Context(), verifier(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, v)
	})
	check := func(level economy.VerificationLevel) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			who, ok := storedanswers.OwnerOrAdmin(req.Context())
			if !ok {
				writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may verify it")
				return
			}
			var in economy.VerificationRequest
			if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "body must be what the check confirms, as JSON: "+err.Error())
				return
			}
			in.Level = level
			ws, kyc := chi.URLParam(req, "wsID"), verifier()
			c, err := store.StartVerification(req.Context(), kyc, ws, who, in)
			switch {
			case errors.Is(err, economy.ErrVerificationInvalid):
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			case errors.Is(err, economy.ErrVerificationOrder):
				writeJSONErr(w, http.StatusConflict, err.Error())
				return
			case err != nil:
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			v, err := store.Verification(req.Context(), kyc, ws)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, map[string]any{"check": c, "verification": v})
		}
	}
	r.Post("/v1/workspaces/{wsID}/verification/contact", check(economy.LevelContact))
	r.Post("/v1/workspaces/{wsID}/verification/identity", check(economy.LevelIdentity))
	r.Post("/v1/workspaces/{wsID}/verification/company", check(economy.LevelCompany))
}
