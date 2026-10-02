package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/billing"
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

// creditLineInvoicers turns on credit line invoicing and returns the Services that invoice: the main one, on
// the main key, and the test workspaces' own, on the test-mode key — each only its own kind of company's
// (B26.4). With none of their own while the main key is live, a test company's draws are refused, naming
// what to set, rather than go on a live invoice.
func creditLineInvoicers(main *billing.Service, mainStripe *billing.LiveStripe, mainKeyLive bool, test *billing.Service, testStripe *billing.LiveStripe) []creditLineInvoicer {
	bills := []creditLineInvoicer{main.WithCreditLines(mainStripe)}
	switch {
	case test != nil:
		bills = append(bills, test.WithCreditLines(testStripe))
	case mainKeyLive:
		main.RefuseTestCreditLines("LENS_STRIPE_TEST_SECRET_KEY and LENS_STRIPE_TEST_WEBHOOK_SECRET")
	}
	return bills
}

// invoiceCreditLines puts each company's last month of credit line draws on a Stripe invoice, hourly, so a
// month's invoice goes out within the hour of its first day.
func invoiceCreditLines(ctx context.Context, bills ...creditLineInvoicer) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		for _, b := range bills {
			if n, err := b.InvoiceCreditLines(ctx, time.Now()); err != nil {
				slog.Warn("credit lines: invoicing", "invoiced", n, "err", err)
			} else if n > 0 {
				slog.Info("credit lines: invoiced", "invoices", n)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
