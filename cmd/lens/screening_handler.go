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

// operatorAlerter delivers an alert to the operator: *modelwatch.WebhookNotifier, at LENS_OPERATOR_ALERT_WEBHOOK_URL.
type operatorAlerter interface {
	NotifyAs(ctx context.Context, kind, source, subject, body string) error
}

// sanctionsListAlert alerts the operator when a sanctions list does not download: at once, again when what failed
// changes, and otherwise once a day while it keeps failing (a failed list is retried each hour).
type sanctionsListAlert struct {
	sink    operatorAlerter // nil when no sink is configured: the failure is only logged
	lastMsg string
	lastAt  time.Time
}

func (a *sanctionsListAlert) failed(ctx context.Context, err error, now time.Time) {
	slog.Error("screening: a sanctions list did not download; the copy already loaded stays in force", "err", err)
	msg := err.Error()
	if msg == a.lastMsg && now.Sub(a.lastAt) < 24*time.Hour {
		return
	}
	if a.sink == nil {
		slog.Error("screening: no operator alert sink is configured (LENS_OPERATOR_ALERT_WEBHOOK_URL and LENS_OPERATOR_ALERT_WEBHOOK_SECRET); nobody is told the sanctions list is stale")
		return
	}
	if nerr := a.sink.NotifyAs(ctx, "sanctions_list_stale", "lens/internal/screening",
		"A sanctions list did not download",
		"Payments are still screened, against the copy already loaded. "+msg+". See GET /v1/admin/screening."); nerr != nil {
		slog.Error("screening: the operator alert did not send", "err", nerr)
		return
	}
	a.lastMsg, a.lastAt = msg, now
}

// refreshSanctionsLists keeps the sanctions lists current: each is downloaded once a day, and a failed download keeps
// the copy in force, alerts the operator, and is shown stale at GET /v1/admin/screening until one succeeds.
func refreshSanctionsLists(ctx context.Context, store *screening.Store, alert *sanctionsListAlert) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		if err := store.RefreshDue(ctx); err != nil {
			alert.failed(ctx, err, time.Now())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
