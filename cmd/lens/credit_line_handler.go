package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
)

// B22.4 — Talyvor's credit line for companies: spend now, pay monthly (economy/credit_line.go).
//
//	GET /v1/workspaces/{wsID}/credit-line   the company's limit, what it has used and can still use, whether
//	                                        it is paused and why, and its monthly invoices
//
// Mounted in the authed group; the operator sets the line (`lens credit-lines`).

type creditLineReader interface {
	CreditLine(ctx context.Context, workspaceID string) (economy.CreditLine, error)
}

func mountCreditLineRoutes(r chi.Router, store creditLineReader) {
	r.Get("/v1/workspaces/{wsID}/credit-line", func(w http.ResponseWriter, req *http.Request) {
		l, err := store.CreditLine(req.Context(), chi.URLParam(req, "wsID"))
		if errors.Is(err, economy.ErrNoCreditLine) {
			writeJSONErr(w, http.StatusNotFound, "this workspace has no credit line")
			return
		}
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, l)
	})
}

// creditLineInvoicer is billing's monthly credit line invoicing (*billing.Service).
type creditLineInvoicer interface {
	InvoiceCreditLines(ctx context.Context, now time.Time) (int, error)
}

// invoiceCreditLines puts each company's last month of credit line draws on a Stripe invoice, hourly, so a
// month's invoice goes out within the hour of its first day.
func invoiceCreditLines(ctx context.Context, billing creditLineInvoicer) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := billing.InvoiceCreditLines(ctx, time.Now()); err != nil {
			slog.Warn("credit lines: invoicing", "invoiced", n, "err", err)
		} else if n > 0 {
			slog.Info("credit lines: invoiced", "invoices", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
