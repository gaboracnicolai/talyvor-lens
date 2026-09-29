package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	stripe "github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/invoice"
	"github.com/stripe/stripe-go/v81/invoiceitem"
)

// credit_line.go — B22.4: A COMPANY'S CREDIT LINE IS INVOICED MONTHLY THROUGH STRIPE.
//
// On the first of each month (UTC) — and on any later run that finds some left — the draws a company's credit
// line lent before that day and never invoiced go onto ONE Stripe invoice, priced at the LXC peg and sent for
// payment within creditLineDaysUntilDue days (economy/credit_line.go has the line itself). When it is paid
// (invoice.paid) its draws stop counting against the limit; unpaid past its due date, it pauses the line.

// creditLineDaysUntilDue is how long a company has to pay a month's credit line invoice.
const creditLineDaysUntilDue = 14

// creditLineStripeAPI is the credit line's half of the Stripe seam. *LiveStripe satisfies it.
type creditLineStripeAPI interface {
	// CreateCreditLineInvoice creates and finalizes one invoice of cents for the customer, idempotent on
	// idempotencyKey, and returns its id and due date.
	CreateCreditLineInvoice(ctx context.Context, customerID, workspaceID string, cents int64, description, idempotencyKey string) (string, time.Time, error)
}

// WithCreditLines turns on the monthly invoicing of company credit lines.
func (s *Service) WithCreditLines(api creditLineStripeAPI) *Service {
	s.creditLineStripe = api
	return s
}

// InvoiceCreditLines invoices, for every company, the credit line draws made before the start of now's month
// and not yet invoiced, and returns how many invoices it created.
func (s *Service) InvoiceCreditLines(ctx context.Context, now time.Time) (int, error) {
	if s.creditLineStripe == nil {
		return 0, nil
	}
	now = now.UTC()
	periodEnd := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT workspace_id FROM credit_line_draws WHERE invoice_id = '' AND drawn_at < $1`, periodEnd)
	if err != nil {
		return 0, fmt.Errorf("billing: credit line draws: %w", err)
	}
	var workspaces []string
	for rows.Next() {
		var ws string
		if err := rows.Scan(&ws); err != nil {
			rows.Close()
			return 0, err
		}
		workspaces = append(workspaces, ws)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, ws := range workspaces {
		made, err := s.invoiceCreditLine(ctx, ws, periodEnd)
		if err != nil {
			errs = append(errs, fmt.Errorf("billing: credit line invoice for %s: %w", ws, err))
			continue
		}
		if made {
			n++
		}
	}
	return n, errors.Join(errs...)
}

// invoiceCreditLine puts workspaceID's uninvoiced draws before periodEnd on one Stripe invoice. The draws are
// held while the invoice is made; a retry after a failed commit reuses the idempotency key, so Stripe returns
// the same invoice rather than a second.
func (s *Service) invoiceCreditLine(ctx context.Context, workspaceID string, periodEnd time.Time) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT id, amount_ulxc FROM credit_line_draws
		WHERE workspace_id = $1 AND invoice_id = '' AND drawn_at < $2 ORDER BY drawn_at FOR UPDATE`, workspaceID, periodEnd)
	if err != nil {
		return false, err
	}
	var ids []string
	var ulxc int64
	for rows.Next() {
		var id string
		var amount int64
		if err := rows.Scan(&id, &amount); err != nil {
			rows.Close()
			return false, err
		}
		ids, ulxc = append(ids, id), ulxc+amount
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	cents := (ulxc + ulxcPerCent/2) / ulxcPerCent // to the nearest cent, at the peg
	if cents <= 0 {
		return false, nil // under half a cent: it waits for next month's
	}
	customerID, err := s.ensureCustomer(ctx, workspaceID)
	if err != nil {
		return false, err
	}
	month := periodEnd.AddDate(0, -1, 0).Format("January 2006")
	stripeID, dueAt, err := s.creditLineStripe.CreateCreditLineInvoice(ctx, customerID, workspaceID, cents,
		"Talyvor credit line, used through "+month, "credit-line-"+workspaceID+"-"+periodEnd.Format("2006-01"))
	if err != nil {
		return false, err
	}
	id := "cli_" + uuid.NewString()
	if _, err := tx.Exec(ctx, `INSERT INTO credit_line_invoices (id, workspace_id, period_end, amount_ulxc, amount_cents, stripe_invoice_id, due_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, id, workspaceID, periodEnd, ulxc, cents, stripeID, dueAt); err != nil {
		return false, fmt.Errorf("record invoice %s: %w", stripeID, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE credit_line_draws SET invoice_id = $1 WHERE id = ANY($2)`, id, ids); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// creditLineInvoicePaid records a paid credit line invoice and reports whether the event was one. The line
// counts its draws as repaid from here, and a late invoice stops pausing it.
func (s *Service) creditLineInvoicePaid(w http.ResponseWriter, ctx context.Context, event *stripe.Event) bool {
	var inv struct {
		ID                string `json:"id"`
		StatusTransitions struct {
			PaidAt int64 `json:"paid_at"`
		} `json:"status_transitions"`
	}
	if err := json.Unmarshal(event.Data.Raw, &inv); err != nil || inv.ID == "" {
		return false
	}
	paidAt := time.Unix(event.Created, 0).UTC()
	if inv.StatusTransitions.PaidAt > 0 {
		paidAt = time.Unix(inv.StatusTransitions.PaidAt, 0).UTC()
	}
	tag, err := s.pool.Exec(ctx, `UPDATE credit_line_invoices SET paid_at = COALESCE(paid_at, $2) WHERE stripe_invoice_id = $1`, inv.ID, paidAt)
	if err != nil {
		s.fail(w, "credit line invoice paid", event.ID, err)
		return true
	}
	if tag.RowsAffected() == 0 {
		return false
	}
	w.WriteHeader(http.StatusOK)
	return true
}

// CreateCreditLineInvoice creates a draft invoice of one line of cents, finalizes it and returns its id and
// due date. Pending invoice items are left out, so the invoice carries the credit line and nothing else.
func (l *LiveStripe) CreateCreditLineInvoice(ctx context.Context, customerID, workspaceID string, cents int64, description, idempotencyKey string) (string, time.Time, error) {
	ip := &stripe.InvoiceParams{
		Customer:                    stripe.String(customerID),
		CollectionMethod:            stripe.String(string(stripe.InvoiceCollectionMethodSendInvoice)),
		DaysUntilDue:                stripe.Int64(creditLineDaysUntilDue),
		PendingInvoiceItemsBehavior: stripe.String("exclude"),
		Currency:                    stripe.String(string(stripe.CurrencyUSD)),
		Description:                 stripe.String(description),
	}
	ip.Context = ctx
	ip.AddMetadata("credit_line_workspace_id", workspaceID)
	ip.SetIdempotencyKey(idempotencyKey + "-invoice")
	draft, err := invoice.New(ip)
	if err != nil {
		return "", time.Time{}, err
	}
	item := &stripe.InvoiceItemParams{
		Customer:    stripe.String(customerID),
		Invoice:     stripe.String(draft.ID),
		Amount:      stripe.Int64(cents),
		Currency:    stripe.String(string(stripe.CurrencyUSD)),
		Description: stripe.String(description),
	}
	item.Context = ctx
	item.SetIdempotencyKey(idempotencyKey + "-item")
	if _, err := invoiceitem.New(item); err != nil {
		return "", time.Time{}, err
	}
	fp := &stripe.InvoiceFinalizeInvoiceParams{}
	fp.Context = ctx
	fp.SetIdempotencyKey(idempotencyKey + "-finalize")
	final, err := invoice.FinalizeInvoice(draft.ID, fp)
	if err != nil {
		return "", time.Time{}, err
	}
	return final.ID, time.Unix(final.DueDate, 0).UTC(), nil
}
