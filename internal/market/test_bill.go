package market

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// test_bill.go — B25.7: A TEST BUYER'S BILL PAID, ITS EARNINGS PAST THE HOLDBACK, AND REFUNDED, INSIDE ONE TESTER RUN.
//
// A buyer's marketplace uses clear when their monthly bill is paid, and the seller's earnings wait the 14-day
// holdback after that; a paid bill is refunded when Stripe says so (charge.refunded). A tester run lasts
// minutes, so for a TEST (synthetic) buyer only, PayTestBill pays the bill now — recorded as paid one holdback
// ago, since an earning is append-only and its payable_at is fixed when it clears — and RefundTestBill refunds
// a paid one as charge.refunded does (ReverseInvoice). A real buyer is never touched.

// ErrNotTestBuyer: the buyer is not a test workspace; nothing was paid or refunded.
var ErrNotTestBuyer = errors.New("market: only a test (synthetic) workspace's bill can be paid or refunded here")

// ErrNoTestBill: no paid bill of this test buyer has that id.
var ErrNoTestBill = errors.New("market: no paid bill of this test workspace has that id")

// PayTestBill pays a test buyer's marketplace bill now: every billed use metered and not yet cleared clears on
// one invoice (in_synthetic_…), paid Holdback before now, so its sellers' earnings are payable at now — the
// payout run or take-as-credits then pays them. It answers the invoice and how many uses it cleared.
func (s *Store) PayTestBill(ctx context.Context, buyerWorkspaceID string, now time.Time) (string, int, error) {
	var test bool
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT synthetic FROM workspaces WHERE id = $1), false)`, buyerWorkspaceID).Scan(&test); err != nil {
		return "", 0, fmt.Errorf("market: pay a test bill: %w", err)
	}
	if !test {
		return "", 0, ErrNotTestBuyer
	}
	invoiceID := "in_synthetic_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	n, err := s.ClearInvoice(ctx, buyerWorkspaceID, invoiceID, time.Unix(0, 0).UTC(), now.Add(time.Hour), now.Add(-Holdback), false)
	if err != nil || n == 0 {
		invoiceID = ""
	}
	return invoiceID, n, err
}

// RefundTestBill refunds a test buyer's paid bill in full, as Stripe's charge.refunded refunds one: a
// buyer_refund market_refunds row per use it cleared, each seller's share reversed. It answers how many uses
// were refunded now (a bill refunded before refunds nothing more).
func (s *Store) RefundTestBill(ctx context.Context, buyerWorkspaceID, invoiceID string) (int, error) {
	var mine, all int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE buyer_workspace_id = $2 AND test), count(*)
		FROM market_uses WHERE cleared_invoice_id = $1`, invoiceID, buyerWorkspaceID).Scan(&mine, &all); err != nil {
		return 0, fmt.Errorf("market: refund a test bill: %w", err)
	}
	if all == 0 || mine != all {
		return 0, ErrNoTestBill
	}
	n, _, err := s.ReverseInvoice(ctx, invoiceID, "buyer_refund", "ch_synthetic_"+strings.ReplaceAll(uuid.NewString(), "-", ""), 0, true)
	return n, err
}
