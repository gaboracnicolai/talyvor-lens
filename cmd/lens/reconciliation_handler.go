package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
)

// reconciliation_handler.go — B30.11: DAILY RECONCILIATION AGAINST THE PARTNER, AND THE SAFEGUARDING VIEW.
//
//	GET /v1/admin/reconciliation   each day's run in each currency, newest first, with every break — a payment the
//	                               partner's statement does not show (missing), one only it shows (extra), or one
//	                               whose amounts differ
//	GET /v1/admin/safeguarding     the latest run in each currency: what customers hold against what the partner
//	                               reports holding for them, and any shortfall

// reconciliations is what the operator reads of reconciliation: *economy.DualTokenStore.
type reconciliations interface {
	ReconciliationRuns(ctx context.Context, limit int) ([]economy.ReconciliationRun, error)
	Safeguarding(ctx context.Context) ([]economy.ReconciliationRun, error)
}

func newReconciliationRunsHandler(r reconciliations) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		runs, err := r.ReconciliationRuns(req.Context(), 200)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"runs": runs})
	})
}

func newSafeguardingHandler(r reconciliations) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		currencies, err := r.Safeguarding(req.Context())
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"currencies": currencies})
	})
}

// reconcileDaily is the daily job: once each UTC day — the first hour it runs on a day — yesterday's postings through
// partner accounts are reconciled with the account partner's statements, and any break is sent to the operator (sink
// nil: only logged). A run that fails is tried again the next hour.
func reconcileDaily(ctx context.Context, store *economy.DualTokenStore, registry *partners.Registry, sink operatorAlerter) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	done := ""
	for {
		now := time.Now().UTC()
		if day := now.Format(time.DateOnly); day != done {
			if err := reconcileOnce(ctx, store, registry, sink, now.Add(-24*time.Hour)); err != nil {
				slog.Warn("reconciliation: the daily run did not complete", "err", err)
			} else {
				done = day
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func reconcileOnce(ctx context.Context, store *economy.DualTokenStore, registry *partners.Registry, sink operatorAlerter, day time.Time) error {
	p, err := registry.Account(ctx, economy.CapabilityCurrencyAccounts)
	if err != nil {
		return err
	}
	runs, err := store.ReconcileDay(ctx, p, day)
	if err != nil {
		return err
	}
	var found []string
	for _, r := range runs {
		if r.BreakCount > 0 {
			found = append(found, fmt.Sprintf("%s: %d breaks", r.Currency, r.BreakCount))
		}
	}
	if len(found) == 0 {
		slog.Info("reconciliation: the partner's statements match the ledger", "day", day.Format(time.DateOnly))
		return nil
	}
	summary := strings.Join(found, ", ")
	slog.Error("reconciliation: the partner's statements and the ledger disagree", "day", day.Format(time.DateOnly), "breaks", summary)
	if sink == nil {
		slog.Error("reconciliation: no operator alert sink is configured (LENS_OPERATOR_ALERT_WEBHOOK_URL and LENS_OPERATOR_ALERT_WEBHOOK_SECRET); nobody is told of the breaks")
		return nil
	}
	if err := sink.NotifyAs(ctx, "reconciliation_break", "lens/internal/economy",
		"Reconciliation found breaks for "+day.Format(time.DateOnly),
		"The partner's statements and the ledger disagree — "+summary+". See GET /v1/admin/reconciliation."); err != nil {
		slog.Error("reconciliation: the operator alert did not send", "err", err)
	}
	return nil
}
