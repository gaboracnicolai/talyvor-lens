package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/screening"
)

// screening_handler.go — B30.6: SANCTIONS SCREENING, AS THE OPERATOR SEES IT.
//
//	GET  /v1/admin/screening                       each sanctions list — entries, when it was loaded, and a failed
//	                                               download's reason (stale) — and the compliance cases, newest
//	                                               first, only those in ?status= when it is given
//	POST /v1/admin/screening/cases/{caseID}/release a held case's close match is not them: the money may move on retry
//	POST /v1/admin/screening/cases/{caseID}/refuse  it is them: the money never moves
//
// The decision records who made it: the operator X-Talyvor-Operator names, or "admin".

// screeningLists is what the operator reads of the lists: *screening.Store.
type screeningLists interface {
	Lists(ctx context.Context) ([]screening.ListStatus, error)
	ThresholdBPS() int
}

// screeningCases is the compliance cases: *screening.Screener.
type screeningCases interface {
	Cases(ctx context.Context, status string, limit int) ([]screening.Case, error)
	Decide(ctx context.Context, id string, release bool, by, note string) (screening.Case, error)
}

func newScreeningOverviewHandler(lists screeningLists, cases screeningCases) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		status := strings.TrimSpace(req.URL.Query().Get("status"))
		switch status {
		case "", screening.CaseBlocked, screening.CaseHeld, screening.CaseReleased, screening.CaseRefused:
		default:
			writeJSONErr(w, http.StatusBadRequest, "status is blocked, held, released or refused")
			return
		}
		ls, err := lists.Lists(req.Context())
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		cs, err := cases.Cases(req.Context(), status, 500)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"lists": ls, "cases": cs, "fuzzy_threshold_bps": lists.ThresholdBPS()})
	})
}

func newScreeningDecideHandler(cases screeningCases, release bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Note string `json:"note"`
		}
		if req.ContentLength != 0 {
			if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
		}
		by := strings.TrimSpace(req.Header.Get(moderatorOperatorHeader))
		if by == "" {
			by = "admin"
		}
		c, err := cases.Decide(req.Context(), chi.URLParam(req, "caseID"), release, by, strings.TrimSpace(in.Note))
		switch {
		case errors.Is(err, screening.ErrCaseNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, screening.ErrCaseDecided):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, map[string]any{"case": c})
		}
	})
}

// refreshSanctionsLists keeps the sanctions lists current: each is downloaded once a day, and a failed download keeps
// the copy in force and is logged as an error, and shown stale at GET /v1/admin/screening, until one succeeds.
func refreshSanctionsLists(ctx context.Context, store *screening.Store) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		if err := store.RefreshDue(ctx); err != nil {
			slog.Error("screening: a sanctions list did not download; the copy already loaded stays in force", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
