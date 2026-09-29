package billing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/talyvor/lens/internal/economy"
)

// fakeCreditLineStripe records the credit line invoices billing asks Stripe for.
type fakeCreditLineStripe struct {
	cents []int64
	due   time.Time
}

func (f *fakeCreditLineStripe) CreateCreditLineInvoice(_ context.Context, _, workspaceID string, cents int64, _, _ string) (string, time.Time, error) {
	f.cents = append(f.cents, cents)
	return "in_" + workspaceID, f.due, nil
}

// B22.4 — on the migrated schema: a private user's workspace is refused a credit line as class RED; a
// company's agent holding nothing spends past zero within its line (each draw an lxc_ledger row) and not past
// it; the month's Stripe invoice carries exactly the used amount; unpaid past its due date the line is paused
// and lends nothing, and paying it (invoice.paid) lets it lend again with the paid draws repaid.
func TestCreditLine_CompanySpendsPastZeroIsInvoicedAndAMissedPaymentPausesIt(t *testing.T) {
	svc, pool, dt := newBillingService(t)
	ctx := context.Background()
	const lxc = int64(1_000_000)
	run := uuid.NewString()[:8]
	company, person := "ws-co-"+run, "ws-person-"+run
	seedWS(t, pool, company)
	seedWS(t, pool, person)
	if _, err := dt.SetCreditLine(ctx, person, 100*lxc, "test"); !errors.Is(err, economy.ErrCreditLineCompaniesOnly) || !strings.Contains(err.Error(), "class RED") {
		t.Fatalf("a credit line for a private user's workspace = %v, want refused as class RED", err)
	}
	if err := dt.SetCompany(ctx, company, true); err != nil {
		t.Fatal(err)
	}
	if _, err := dt.SetCreditLine(ctx, company, 100*lxc, "test"); err != nil {
		t.Fatal(err)
	}
	agent, err := dt.CreateAgent(ctx, company, "buyer", "user-co")
	if err != nil {
		t.Fatal(err)
	}
	key := "key-" + run
	if err := dt.AttachAgentKey(ctx, company, agent.ID, key); err != nil {
		t.Fatal(err)
	}
	spend := func(amount int64) error {
		return dt.SpendLXCForAgent(ctx, key, company, uuid.NewString(), amount, "a model call", economy.AgentDebitMeta{})
	}

	// The agent holds nothing: 30 and then 70 LXC are lent; 1 more would pass the 100 LXC limit.
	if err := spend(30 * lxc); err != nil {
		t.Fatalf("spending 30 LXC past zero on the line = %v", err)
	}
	if err := spend(71 * lxc); !errors.Is(err, economy.ErrSubBudgetExceeded) {
		t.Fatalf("spending past the line = %v, want refused", err)
	}
	if err := spend(70 * lxc); err != nil {
		t.Fatal(err)
	}
	var drawn, spent int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount) FILTER (WHERE type = 'credit_line_draw'), 0)::bigint,
		COALESCE(-sum(amount) FILTER (WHERE type = 'spend'), 0)::bigint FROM lxc_ledger WHERE workspace_id = $1`, company).Scan(&drawn, &spent); err != nil {
		t.Fatal(err)
	}
	if drawn != 100*lxc || spent != 100*lxc || balance(t, pool, company) != 0 {
		t.Fatalf("ledger: %d drawn, %d spent, balance %d; want 100 LXC drawn and spent, 0 left", drawn, spent, balance(t, pool, company))
	}

	// Next month the invoice carries the 100 LXC used: $10.00. A second run makes no second invoice.
	stripe := &fakeCreditLineStripe{due: time.Now().Add(-time.Hour)} // already past due: a missed payment
	svc = svc.WithCreditLines(stripe)
	nextMonth := time.Now().UTC().AddDate(0, 1, 0)
	for range 2 {
		if _, err := svc.InvoiceCreditLines(ctx, nextMonth); err != nil {
			t.Fatal(err)
		}
	}
	var invoiced, cents int64
	if err := pool.QueryRow(ctx, `SELECT amount_ulxc, amount_cents FROM credit_line_invoices WHERE workspace_id = $1`, company).Scan(&invoiced, &cents); err != nil {
		t.Fatal(err)
	}
	if len(stripe.cents) != 1 || stripe.cents[0] != 1000 || invoiced != 100*lxc || cents != 1000 {
		t.Fatalf("invoices sent %v, recorded %d µLXC for %d¢; want one of 1000¢ for 100 LXC", stripe.cents, invoiced, cents)
	}

	// Unpaid past due, the line is paused even with room under a raised limit.
	if _, err := dt.SetCreditLine(ctx, company, 200*lxc, "test"); err != nil {
		t.Fatal(err)
	}
	if err := spend(10 * lxc); !errors.Is(err, economy.ErrSubBudgetExceeded) {
		t.Fatalf("spending on a line with a late invoice = %v, want refused", err)
	}
	if l, err := dt.CreditLine(ctx, company); err != nil || !l.Paused || l.AvailableULXC != 0 || !strings.Contains(l.PausedReason, "unpaid") {
		t.Fatalf("line with a late invoice = %+v, %v; want paused for the unpaid invoice", l, err)
	}

	// Paid: the line lends again, and only the new draw is used.
	body, sig := signed(testWebhookSecret, "evt_"+run, "invoice.paid", map[string]any{"id": "in_" + company})
	if code := post(svc, body, sig); code != 200 {
		t.Fatalf("invoice.paid = %d", code)
	}
	if err := spend(10 * lxc); err != nil {
		t.Fatalf("spending after paying = %v", err)
	}
	if l, err := dt.CreditLine(ctx, company); err != nil || l.Paused || l.UsedULXC != 10*lxc || l.AvailableULXC != 190*lxc {
		t.Fatalf("line after paying = %+v, %v; want 10 LXC used and 190 available", l, err)
	}
}
