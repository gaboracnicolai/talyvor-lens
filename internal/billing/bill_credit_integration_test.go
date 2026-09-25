package billing

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/mining"
)

// B13.2 — a subscriber's final royalty earnings come off their next renewal invoice, capped at their fee;
// never a payout. Asserted on the claim row, the LENS ledger row and balance, and the invoice line.

type fakeInvoiceCrediter struct {
	lines map[string]int64 // invoice → credited cents
	keys  map[string]bool
}

func (f *fakeInvoiceCrediter) CreditInvoice(_ context.Context, _, invoiceID string, amountCents int64, _, key string) error {
	if f.keys[key] {
		return nil // Stripe's idempotency: the same key adds nothing
	}
	f.keys[key] = true
	f.lines[invoiceID] += amountCents
	return nil
}

const lensULENS = 1_000_000 // µLENS per LENS

// earner is a subscribed workspace with `final` LENS of final pool royalties (also its LENS balance) and
// `held` LENS still held.
func earner(t *testing.T, svc *Service, pool *pgxpool.Pool, ws string, final, held int64) {
	t.Helper()
	ctx := context.Background()
	seedWS(t, pool, ws)
	end := time.Now().Add(30 * 24 * time.Hour)
	b, sig := signedAt(testWebhookSecret, "evt_sub_"+ws, "customer.subscription.created", time.Now(),
		subObj("sub_"+ws, ws, "cus_"+ws, "price_test_model2", "active", end, false))
	if code := postEvent(svc, b, sig); code != http.StatusOK {
		t.Fatalf("subscription.created → %d", code)
	}
	for status, lens := range map[string]int64{"final": final, "held": held} {
		if _, err := pool.Exec(ctx, `INSERT INTO pool_royalty_mints
			(request_id, requester_workspace_id, contributor_workspace_id, layer, minted_amount, status)
			VALUES ($1, 'ws-asker', $2, 'exact', $3, $4)`, fmt.Sprintf("%s-%s", ws, status), ws, lens*lensULENS, status); err != nil {
			t.Fatal(err)
		}
	}
	if err := mining.NewLedgerStore(pool).Credit(ctx, ws, final*lensULENS, "pool_royalty", "finalized royalty", nil); err != nil {
		t.Fatal(err)
	}
}

func renewal(t *testing.T, svc *Service, eventID, invoiceID, ws string) int {
	t.Helper()
	b, sig := signedAt(testWebhookSecret, eventID, "invoice.created", time.Now(), map[string]any{
		"id": invoiceID, "object": "invoice", "billing_reason": "subscription_cycle", "status": "draft",
		"customer": "cus_" + ws, "subscription": "sub_" + ws, "total": 2000, "currency": "usd",
	})
	return postEvent(svc, b, sig)
}

func TestBillCredit_EarningsComeOffTheNextInvoice_CappedAtTheFee(t *testing.T) {
	svc, pool, _ := newSubService(t)
	inv := &fakeInvoiceCrediter{lines: map[string]int64{}, keys: map[string]bool{}}
	svc = svc.WithBillCredits(mining.NewLedgerStore(pool), inv)
	ctx := context.Background()
	stamp := time.Now().UnixNano()

	for _, tc := range []struct {
		name               string
		finalLENS, held    int64
		wantCents          int64 // the negative line
		wantInvoice        int64 // $20 − the line
		wantDebitLENS      int64
		wantBalanceLeftLNS int64
	}{
		{"earned 60 LENS", 60, 40, 600, 1400, 60, 0},   // $6 back → next invoice $14; held 40 not counted
		{"earned far more", 500, 0, 2000, 0, 200, 300}, // capped at the $20 fee → $0; 300 LENS stay, not paid out
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := fmt.Sprintf("ws-bc-%d-%d", tc.finalLENS, stamp)
			earner(t, svc, pool, ws, tc.finalLENS, tc.held)
			invoiceID := "in_" + ws
			if code := renewal(t, svc, "evt_inv_"+ws, invoiceID, ws); code != http.StatusOK {
				t.Fatalf("invoice.created → %d", code)
			}
			if got := inv.lines[invoiceID]; got != tc.wantCents || 2000-got != tc.wantInvoice {
				t.Errorf("invoice credit = %d¢ (invoice %d¢), want %d¢ (invoice %d¢)", got, 2000-got, tc.wantCents, tc.wantInvoice)
			}
			var claimCents, claimULENS int64
			if err := pool.QueryRow(ctx, `SELECT credited_usd_cents, credited_ulens FROM subscription_bill_credits WHERE invoice_id = $1`,
				invoiceID).Scan(&claimCents, &claimULENS); err != nil {
				t.Fatalf("claim row: %v", err)
			}
			var debit int64
			if err := pool.QueryRow(ctx, `SELECT COALESCE(SUM(-amount),0) FROM lens_token_ledger WHERE workspace_id = $1 AND type = $2`,
				ws, TypeSubscriptionBillCredit).Scan(&debit); err != nil {
				t.Fatal(err)
			}
			bal, err := mining.NewLedgerStore(pool).GetBalance(ctx, ws)
			if err != nil {
				t.Fatal(err)
			}
			if claimCents != tc.wantCents || claimULENS != tc.wantDebitLENS*lensULENS || debit != claimULENS || bal != tc.wantBalanceLeftLNS*lensULENS {
				t.Errorf("claim %d¢ / %d µLENS, ledger debit %d, balance left %d; want %d¢ / %d LENS, balance %d LENS",
					claimCents, claimULENS, debit, bal, tc.wantCents, tc.wantDebitLENS, tc.wantBalanceLeftLNS)
			}

			// Stripe redelivers (a new event id for the same invoice): nothing is credited twice.
			if code := renewal(t, svc, "evt_inv_again_"+ws, invoiceID, ws); code != http.StatusOK {
				t.Fatalf("redelivery → %d", code)
			}
			if got := inv.lines[invoiceID]; got != tc.wantCents {
				t.Errorf("after redelivery the invoice credit is %d¢, want %d¢", got, tc.wantCents)
			}
			var claims int
			if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM lens_token_ledger WHERE workspace_id = $1 AND type = $2`,
				ws, TypeSubscriptionBillCredit).Scan(&claims); err != nil || claims != 1 {
				t.Errorf("bill-credit ledger rows after redelivery = %d (%v), want 1", claims, err)
			}
		})
	}
}
