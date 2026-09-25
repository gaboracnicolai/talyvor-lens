package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
	stripe "github.com/stripe/stripe-go/v81"

	"github.com/talyvor/lens/internal/economy"
)

// bill_credit.go — B13.2: subscribers earn, and their earnings come off the next bill.
//
// When another user's question is answered from a subscriber's answer, the subscriber is minted half of
// what that user paid, in LENS (pool and distill royalties). At renewal — Stripe's invoice.created for a
// DRAFT subscription_cycle invoice, the only moment lines can still be added — the subscriber's FINAL
// (not held) royalty earnings not yet credited are turned into a negative line on that invoice:
//
//	credit = min(earnings at the LENS peg, the subscriber's own fee, the invoice's total)
//
// so an invoice is never negative and a subscriber never earns back more than they pay. The LENS the
// credit consumed is debited from the workspace's balance in the same transaction as the claim row
// (subscription_bill_credits, one per invoice). Surplus earnings stay as LENS: this is a bill credit,
// never a payout, and LENS stays non-redeemable for cash.

// TypeSubscriptionBillCredit is the lens_token_ledger type of the LENS a bill credit consumed.
const TypeSubscriptionBillCredit = "subscription_bill_credit"

// lensDebiter debits LENS inside the caller's transaction. *mining.LedgerStore satisfies it.
type lensDebiter interface {
	DebitTx(ctx context.Context, tx pgx.Tx, workspaceID string, amount int64, txType, description string, metadata map[string]interface{}) error
}

// invoiceCrediter adds a credit line to a draft invoice. *LiveStripe satisfies it.
type invoiceCrediter interface {
	CreditInvoice(ctx context.Context, customerID, invoiceID string, amountCents int64, description, idempotencyKey string) error
}

// WithBillCredits turns earnings-off-the-bill on. Unwired, invoice.created is acknowledged and ignored.
func (s *Service) WithBillCredits(lens lensDebiter, invoices invoiceCrediter) *Service {
	s.billLens = lens
	s.billInvoices = invoices
	return s
}

// ulensPerCent is µLENS per US cent at the LENS peg (economy.LENSPerUSD LENS per dollar).
var ulensPerCent = int64(economy.LENSPerUSD) * 1_000_000 / 100

func (s *Service) handleInvoiceCreated(w http.ResponseWriter, ctx context.Context, event *stripe.Event) {
	var inv stripe.Invoice
	if err := json.Unmarshal(event.Data.Raw, &inv); err != nil {
		s.log.Warn("billing webhook: unparseable invoice", "event", event.ID)
		w.WriteHeader(http.StatusOK)
		return
	}
	subID := invoiceSubscriptionID(&inv)
	if s.billLens == nil || s.billInvoices == nil || inv.BillingReason != stripe.InvoiceBillingReasonSubscriptionCycle ||
		inv.Status != stripe.InvoiceStatusDraft || subID == "" || inv.Customer == nil || inv.Total <= 0 {
		w.WriteHeader(http.StatusOK) // not a renewal we can still add a line to
		return
	}
	var wsID string
	err := s.pool.QueryRow(ctx, `SELECT workspace_id FROM subscriptions WHERE stripe_subscription_id = $1`, subID).Scan(&wsID)
	if errors.Is(err, pgx.ErrNoRows) {
		w.WriteHeader(http.StatusOK)
		return
	}
	if err != nil {
		s.fail(w, "invoice workspace lookup", event.ID, err)
		return
	}
	cents, ulens, err := s.billCredit(ctx, wsID, subID, inv.Total)
	if err != nil {
		s.fail(w, "bill credit amount", event.ID, err)
		return
	}
	if cents <= 0 {
		w.WriteHeader(http.StatusOK)
		return
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.fail(w, "bill credit begin", event.ID, err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ct, err := tx.Exec(ctx, `
		INSERT INTO subscription_bill_credits (invoice_id, workspace_id, stripe_subscription_id, credited_ulens, credited_usd_cents)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (invoice_id) DO NOTHING`, inv.ID, wsID, subID, ulens, cents)
	if err != nil {
		s.fail(w, "bill credit claim", event.ID, err)
		return
	}
	if ct.RowsAffected() == 0 {
		w.WriteHeader(http.StatusOK) // this invoice was already credited (a redelivery)
		return
	}
	if err := s.billLens.DebitTx(ctx, tx, wsID, ulens, TypeSubscriptionBillCredit,
		"earnings credited against invoice "+inv.ID, map[string]interface{}{"invoice_id": inv.ID, "usd_cents": cents}); err != nil {
		s.fail(w, "bill credit LENS debit", event.ID, err)
		return
	}
	// Idempotent on the invoice: a retry after a failed commit adds no second line.
	if err := s.billInvoices.CreditInvoice(ctx, inv.Customer.ID, inv.ID, cents,
		"Your team's answers earned this back", "talyvor-bill-credit-"+inv.ID); err != nil {
		s.fail(w, "bill credit invoice line", event.ID, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.fail(w, "bill credit commit", event.ID, err)
		return
	}
	s.log.Info("billing: earnings credited against the renewal", "workspace", wsID, "invoice", inv.ID,
		"usd_cents", cents, "ulens", ulens)
	w.WriteHeader(http.StatusOK)
}

// billCredit is the credit for a renewal invoice of invoiceTotalCents: the workspace's final royalty
// earnings not yet credited, no more than its LENS balance, at the peg, capped at its fee and at the
// invoice. Returns the credit in cents and the µLENS it consumes.
func (s *Service) billCredit(ctx context.Context, workspaceID, subscriptionID string, invoiceTotalCents int64) (cents, ulens int64, err error) {
	var earned, credited, balance, fee int64
	err = s.pool.QueryRow(ctx, `
		SELECT
		  (SELECT COALESCE(SUM(minted_amount), 0)::bigint FROM (
		     SELECT minted_amount FROM pool_royalty_mints WHERE contributor_workspace_id = $1 AND status = 'final'
		     UNION ALL
		     SELECT minted_amount FROM distill_royalty_mints WHERE contributor_workspace_id = $1 AND status = 'final') m),
		  (SELECT COALESCE(SUM(credited_ulens), 0)::bigint FROM subscription_bill_credits WHERE workspace_id = $1),
		  (SELECT COALESCE(MAX(balance), 0)::bigint FROM lens_token_balances WHERE workspace_id = $1),
		  (SELECT COALESCE(MAX(fee_usd_cents), 0)::bigint FROM subscription_allowance
		     WHERE stripe_subscription_id = $2
		       AND period_start = (SELECT MAX(period_start) FROM subscription_allowance WHERE stripe_subscription_id = $2))`,
		workspaceID, subscriptionID).Scan(&earned, &credited, &balance, &fee)
	if err != nil {
		return 0, 0, fmt.Errorf("billing: read earnings for bill credit: %w", err)
	}
	if fee <= 0 {
		fee = invoiceTotalCents // no recorded period fee: the renewal's own amount is the plan's price
	}
	available := min(earned-credited, balance)
	if available <= 0 {
		return 0, 0, nil
	}
	cents = min(available/ulensPerCent, fee, invoiceTotalCents)
	if cents <= 0 {
		return 0, 0, nil
	}
	return cents, cents * ulensPerCent, nil
}
