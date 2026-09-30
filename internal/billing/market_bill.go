package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	stripe "github.com/stripe/stripe-go/v81"
)

// market_bill.go — B20.2: THE BUYER'S MONTHLY MARKETPLACE BILL.
//
// Each buyer workspace's paid marketplace uses are metered onto ONE Stripe usage-based subscription of its
// own (market_bills): a meter event per use, its value the use's price in µLXC and its identifier the use's
// id, so a retried use is never billed twice. The subscription's price is a metered price on that meter
// charging the LXC peg per µLXC, so the buyer pays each listing's price and nothing is ever taken from
// prepaid credits. Stripe invoices it monthly; when that invoice is PAID (invoice.paid), the uses it carried
// clear and their sellers earn (internal/market.ClearInvoice).
//
// The subscription carries `market_workspace_id`, NOT `workspace_id`, in its metadata: handleSubscription
// records every subscription naming a workspace_id as that workspace's PLAN (the subscriptions table), and
// a marketplace bill is not a plan.

// ErrNoMarketBill: the marketplace bill is not configured (LENS_MARKET_BILL_PRICE_ID).
var ErrNoMarketBill = errors.New("billing: no marketplace bill is configured (LENS_MARKET_BILL_PRICE_ID)")

// marketStripeAPI is the marketplace bill's half of the Stripe seam. *LiveStripe satisfies it.
type marketStripeAPI interface {
	CreateMarketSubscription(ctx context.Context, customerID, priceID, workspaceID string) (subscriptionID string, err error)
	SendMeterEvent(ctx context.Context, eventName, customerID, identifier string, value int64, at time.Time) error
	CreditMarketUse(ctx context.Context, customerID, subscriptionID string, cents float64, description, idempotencyKey string) (creditID string, err error)
}

// MarketClearer clears the uses a paid marketplace invoice carried. *market.Store satisfies it.
type MarketClearer interface {
	// livemode is the paid invoice's: its sellers' earnings are real money only when it is (B22.1).
	ClearInvoice(ctx context.Context, buyerWorkspaceID, invoiceID string, periodStart, periodEnd, paidAt time.Time, livemode bool) (int, error)
}

// WithMarketBill turns the marketplace bill on: uses are metered as eventName onto a subscription to
// priceID, and a paid invoice of one clears its uses through clearer.
func (s *Service) WithMarketBill(api marketStripeAPI, priceID, eventName string, clearer MarketClearer) *Service {
	s.marketStripe, s.marketPrice, s.marketEvent, s.marketClearer = api, priceID, eventName, clearer
	return s
}

// MeterMarketUse puts one billed use of ulxc µLXC on the buyer's marketplace bill.
func (s *Service) MeterMarketUse(ctx context.Context, workspaceID, useID string, ulxc int64, at time.Time) error {
	if s.marketStripe == nil || s.marketPrice == "" {
		return ErrNoMarketBill
	}
	customerID, err := s.ensureMarketBill(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("billing: marketplace bill for %s: %w", workspaceID, err)
	}
	if err := s.marketStripe.SendMeterEvent(ctx, s.marketEvent, customerID, useID, ulxc, at); err != nil {
		return fmt.Errorf("billing: meter use %s: %w", useID, err)
	}
	return nil
}

// CreditMarketRefund gives a buyer back one refunded use of ulxc µLXC (B20.4, a taken-down listing): a
// negative line of exactly its price on their next marketplace invoice — to the fraction of a cent, as the
// metered line charged it — idempotent on the use, so a retried pass credits it once.
func (s *Service) CreditMarketRefund(ctx context.Context, workspaceID, useID string, ulxc int64, description string) (string, error) {
	if s.marketStripe == nil || s.marketPrice == "" {
		return "", ErrNoMarketBill
	}
	var customerID, subscriptionID string
	if err := s.pool.QueryRow(ctx, `SELECT stripe_customer_id, stripe_subscription_id FROM market_bills WHERE workspace_id = $1`,
		workspaceID).Scan(&customerID, &subscriptionID); err != nil {
		return "", fmt.Errorf("billing: marketplace bill for %s: %w", workspaceID, err)
	}
	return s.marketStripe.CreditMarketUse(ctx, customerID, subscriptionID, float64(ulxc)/float64(ulxcPerCent), description, "market-refund-"+useID)
}

// ensureMarketBill returns the customer whose marketplace subscription the workspace's uses are metered
// to, subscribing it the first time. Stripe is asked with an idempotency key per workspace, so two first
// uses at once make one subscription.
func (s *Service) ensureMarketBill(ctx context.Context, workspaceID string) (string, error) {
	var customerID string
	err := s.pool.QueryRow(ctx, `SELECT stripe_customer_id FROM market_bills WHERE workspace_id = $1`, workspaceID).Scan(&customerID)
	if err == nil {
		return customerID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	customerID, err = s.ensureCustomer(ctx, workspaceID)
	if err != nil {
		return "", err
	}
	subID, err := s.marketStripe.CreateMarketSubscription(ctx, customerID, s.marketPrice, workspaceID)
	if err != nil {
		return "", err
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO market_bills (workspace_id, stripe_customer_id, stripe_subscription_id)
		VALUES ($1, $2, $3) ON CONFLICT (workspace_id) DO NOTHING`, workspaceID, customerID, subID); err != nil {
		return "", err
	}
	return customerID, nil
}

// marketInvoice is the part of a paid invoice the marketplace reads, parsed from the raw event so both
// the API version stripe-go pins (line.price) and later ones (line.pricing.price_details.price, and the
// subscription under parent.subscription_details) are understood.
type marketInvoice struct {
	ID           string          `json:"id"`
	Subscription json.RawMessage `json:"subscription"`
	Parent       struct {
		SubscriptionDetails struct {
			Subscription string `json:"subscription"`
		} `json:"subscription_details"`
	} `json:"parent"`
	StatusTransitions struct {
		PaidAt int64 `json:"paid_at"`
	} `json:"status_transitions"`
	Lines struct {
		Data []struct {
			Period struct {
				Start int64 `json:"start"`
				End   int64 `json:"end"`
			} `json:"period"`
			Price *struct {
				ID string `json:"id"`
			} `json:"price"`
			Pricing *struct {
				PriceDetails struct {
					Price string `json:"price"`
				} `json:"price_details"`
			} `json:"pricing"`
		} `json:"data"`
	} `json:"lines"`
}

func (inv marketInvoice) subscriptionID() string {
	var id string
	if json.Unmarshal(inv.Subscription, &id) == nil && id != "" {
		return id
	}
	var obj struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(inv.Subscription, &obj) == nil && obj.ID != "" {
		return obj.ID
	}
	return inv.Parent.SubscriptionDetails.Subscription
}

// handleInvoicePaid clears the uses a paid marketplace invoice carried. Any other paid invoice is
// acknowledged and left alone.
func (s *Service) handleInvoicePaid(w http.ResponseWriter, ctx context.Context, event *stripe.Event) {
	if s.creditLineInvoicePaid(w, ctx, event) { // B22.4 — a company paid its credit line
		return
	}
	if s.marketClearer == nil || s.marketPrice == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	var inv marketInvoice
	if err := json.Unmarshal(event.Data.Raw, &inv); err != nil {
		s.log.Warn("billing webhook: unparseable invoice", "event", event.ID)
		w.WriteHeader(http.StatusOK)
		return
	}
	var start, end int64
	for _, line := range inv.Lines.Data {
		price := ""
		if line.Price != nil {
			price = line.Price.ID
		} else if line.Pricing != nil {
			price = line.Pricing.PriceDetails.Price
		}
		if price != s.marketPrice {
			continue
		}
		if start == 0 || line.Period.Start < start {
			start = line.Period.Start
		}
		end = max(end, line.Period.End)
	}
	if end == 0 {
		w.WriteHeader(http.StatusOK) // no marketplace line: not a marketplace bill
		return
	}
	var workspaceID string
	err := s.pool.QueryRow(ctx, `SELECT workspace_id FROM market_bills WHERE stripe_subscription_id = $1`, inv.subscriptionID()).Scan(&workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		s.log.Warn("billing webhook: a paid marketplace invoice of no known bill", "event", event.ID, "invoice", inv.ID)
		w.WriteHeader(http.StatusOK)
		return
	}
	if err != nil {
		s.fail(w, "market bill lookup", event.ID, err)
		return
	}
	// B25.6: a test workspace's bill is the test-mode Service's, a real one's the live Service's.
	if mine, err := s.takes(ctx, workspaceID); err != nil {
		s.fail(w, "workspace kind", event.ID, err)
		return
	} else if !mine {
		s.log.Info("billing webhook: a marketplace invoice of the other kind of workspace — test and real money are kept apart",
			"event", event.ID, "invoice", inv.ID, "workspace", workspaceID)
		w.WriteHeader(http.StatusOK)
		return
	}
	paidAt := time.Unix(event.Created, 0).UTC()
	if inv.StatusTransitions.PaidAt > 0 {
		paidAt = time.Unix(inv.StatusTransitions.PaidAt, 0).UTC()
	}
	n, err := s.marketClearer.ClearInvoice(ctx, workspaceID, inv.ID, time.Unix(start, 0).UTC(), time.Unix(end, 0).UTC(), paidAt,
		event.Livemode)
	if err != nil {
		s.fail(w, "market clear", event.ID, err)
		return
	}
	s.log.Info("billing webhook: marketplace invoice paid", "invoice", inv.ID, "workspace", workspaceID, "uses_cleared", n)
	w.WriteHeader(http.StatusOK)
}
