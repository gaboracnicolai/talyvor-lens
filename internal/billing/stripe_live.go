package billing

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	stripe "github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/billing/meterevent"
	"github.com/stripe/stripe-go/v81/checkout/session"
	"github.com/stripe/stripe-go/v81/customer"
	"github.com/stripe/stripe-go/v81/invoiceitem"
	"github.com/stripe/stripe-go/v81/paymentintent"
	"github.com/stripe/stripe-go/v81/subscription"
)

// LiveStripe is the production stripeAPI implementation. It is NOT exercised by
// unit tests (those inject a fake); the webhook/credit money path is what the
// real-PG tests cover. The secret key is set on the process-global stripe.Key.
type LiveStripe struct {
	successURL string
	cancelURL  string
	// key is the secret key this instance calls with. The checkout, customer, card and subscription calls
	// below pass it on each call, so a test-mode instance (B25.2) and the live one never share a key.
	key string
}

// NewLiveStripe configures the Stripe SDK with the secret key and the
// success/cancel redirect URLs Checkout requires.
func NewLiveStripe(secretKey, successURL, cancelURL string) *LiveStripe {
	stripe.Key = secretKey
	return &LiveStripe{successURL: successURL, cancelURL: cancelURL, key: secretKey}
}

// NewTestModeStripe is a second instance with a Stripe TEST-MODE key, for test (synthetic) workspaces
// (B25.2). It leaves the process-global key, which the live instance set, alone.
func NewTestModeStripe(secretKey, successURL, cancelURL string) *LiveStripe {
	return &LiveStripe{successURL: successURL, cancelURL: cancelURL, key: secretKey}
}

func (l *LiveStripe) backend() stripe.Backend { return stripe.GetBackend(stripe.APIBackend) }

// LiveKey reports whether key is a live-mode Stripe key, which moves real money.
func LiveKey(key string) bool {
	return strings.HasPrefix(key, "sk_live_") || strings.HasPrefix(key, "rk_live_")
}

// Livemode reports whether the configured key is live (B22.1: a live key pays out only live earnings).
func (l *LiveStripe) Livemode() bool { return LiveKey(stripe.Key) }

// CreateCustomer creates a Stripe customer tagged with the workspace id.
func (l *LiveStripe) CreateCustomer(ctx context.Context, workspaceID string) (string, error) {
	params := &stripe.CustomerParams{}
	params.Context = ctx
	params.AddMetadata("workspace_id", workspaceID)
	c, err := customer.Client{B: l.backend(), Key: l.key}.New(params)
	if err != nil {
		return "", err
	}
	return c.ID, nil
}

// CreateCheckoutSession creates a one-off (mode=payment) Checkout Session for a
// single USD line item, stamping workspace_id / lxc_amount / usd_cents into the
// session metadata the webhook later re-verifies (never trusts as truth).
func (l *LiveStripe) CreateCheckoutSession(ctx context.Context, p CheckoutParams) (string, string, error) {
	params := &stripe.CheckoutSessionParams{
		Mode:       stripe.String(string(stripe.CheckoutSessionModePayment)),
		Customer:   stripe.String(p.CustomerID),
		SuccessURL: stripe.String(l.successURL),
		CancelURL:  stripe.String(l.cancelURL),
		LineItems: []*stripe.CheckoutSessionLineItemParams{{
			PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
				Currency:   stripe.String("usd"),
				UnitAmount: stripe.Int64(p.USDCents),
				ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
					Name: stripe.String("LXC usage credit top-up"),
				},
			},
			Quantity: stripe.Int64(1),
		}},
		// B22.2: the customer pays in their own currency and Stripe converts it at its rate (the customer pays
		// its conversion fee); the credits are bought in USD, and Lens converts nothing.
		AdaptivePricing: &stripe.CheckoutSessionAdaptivePricingParams{Enabled: stripe.Bool(true)},
	}
	params.Context = ctx
	params.AddMetadata("workspace_id", p.WorkspaceID)
	params.AddMetadata("lxc_amount", strconv.FormatInt(p.LXCAmount, 10)) // µLXC (SEC-2)
	params.AddMetadata("usd_cents", strconv.FormatInt(p.USDCents, 10))

	sess, err := session.Client{B: l.backend(), Key: l.key}.New(params)
	if err != nil {
		return "", "", err
	}
	return sess.URL, sess.ID, nil
}

// CardFingerprint retrieves the payment intent with its payment method expanded
// and returns the card's stable per-card fingerprint (Stripe's card.fingerprint).
// Returns "" (not an error) when there is no expandable card — the caller treats
// "" as capture-failed and swallows it. U6 PR2 owner-linkage.
func (l *LiveStripe) CardFingerprint(ctx context.Context, paymentIntentID string) (string, error) {
	params := &stripe.PaymentIntentParams{}
	params.Context = ctx
	params.AddExpand("payment_method")
	pi, err := paymentintent.Client{B: l.backend(), Key: l.key}.Get(paymentIntentID, params)
	if err != nil {
		return "", err
	}
	if pi.PaymentMethod == nil || pi.PaymentMethod.Card == nil {
		return "", nil
	}
	return pi.PaymentMethod.Card.Fingerprint, nil
}

// CreateSubscriptionCheckoutSession creates a mode=subscription Checkout Session for
// the configured Price. W4.6.1 step 1.
//
// ⚠ THE METADATA IS STAMPED ON THE SUBSCRIPTION, NOT ONLY ON THE SESSION, and that is
// the difference between this and CreateCheckoutSession above. A one-off webhook reads
// `checkout.session.completed`, which carries the session's own metadata. The
// RECURRING events — customer.subscription.updated a month later, and every renewal
// after that — are about the SUBSCRIPTION object and never see the session again. So
// `workspace_id` goes in SubscriptionData.Metadata, which Stripe copies onto the
// subscription and replays on every future event. Without it, the second month's
// event arrives with no way to attribute it, and handleSubscription's
// "no workspace on file" branch is what a renewal would hit forever.
func (l *LiveStripe) CreateSubscriptionCheckoutSession(ctx context.Context, p SubscriptionParams) (string, string, error) {
	params := &stripe.CheckoutSessionParams{
		Mode:       stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		Customer:   stripe.String(p.CustomerID),
		SuccessURL: stripe.String(l.successURL),
		CancelURL:  stripe.String(l.cancelURL),
		LineItems: []*stripe.CheckoutSessionLineItemParams{{
			Price:    stripe.String(p.PriceID),
			Quantity: stripe.Int64(1),
		}},
		SubscriptionData: &stripe.CheckoutSessionSubscriptionDataParams{},
	}
	params.Context = ctx
	params.AddMetadata("workspace_id", p.WorkspaceID)
	params.SubscriptionData.AddMetadata("workspace_id", p.WorkspaceID)
	sess, err := session.Client{B: l.backend(), Key: l.key}.New(params)
	if err != nil {
		return "", "", err
	}
	return sess.URL, sess.ID, nil
}

// SetCancelAtPeriodEnd flips the subscription's cancel_at_period_end flag. B1.5.
// Stripe answers with the updated subscription and then sends the matching
// customer.subscription.updated to the webhook, which is what records it.
func (l *LiveStripe) SetCancelAtPeriodEnd(ctx context.Context, subscriptionID string, cancel bool) (*stripe.Subscription, error) {
	params := &stripe.SubscriptionParams{CancelAtPeriodEnd: stripe.Bool(cancel)}
	params.Context = ctx
	return subscription.Client{B: l.backend(), Key: l.key}.Update(subscriptionID, params)
}

// ChangeSubscriptionPrice moves a subscription's one item to priceID with proration (B18.14): Stripe credits
// the unused time on the old price and charges the rest of the period on the new one, on the next invoice.
// Stripe answers with the updated subscription and sends customer.subscription.updated, which records it.
func (l *LiveStripe) ChangeSubscriptionPrice(ctx context.Context, subscriptionID, priceID string) (*stripe.Subscription, error) {
	get := &stripe.SubscriptionParams{}
	get.Context = ctx
	cur, err := subscription.Client{B: l.backend(), Key: l.key}.Get(subscriptionID, get)
	if err != nil {
		return nil, err
	}
	if cur.Items == nil || len(cur.Items.Data) == 0 || cur.Items.Data[0] == nil {
		return nil, fmt.Errorf("subscription %s has no item to change", subscriptionID)
	}
	params := &stripe.SubscriptionParams{
		Items:             []*stripe.SubscriptionItemsParams{{ID: stripe.String(cur.Items.Data[0].ID), Price: stripe.String(priceID)}},
		ProrationBehavior: stripe.String("create_prorations"),
	}
	params.Context = ctx
	return subscription.Client{B: l.backend(), Key: l.key}.Update(subscriptionID, params)
}

// CreditInvoice adds a NEGATIVE line of amountCents to a draft invoice (B13.2), idempotent on
// idempotencyKey so a retried webhook cannot add a second one.
func (l *LiveStripe) CreditInvoice(ctx context.Context, customerID, invoiceID string, amountCents int64, description, idempotencyKey string) error {
	params := &stripe.InvoiceItemParams{
		Customer:    stripe.String(customerID),
		Invoice:     stripe.String(invoiceID),
		Amount:      stripe.Int64(-amountCents),
		Currency:    stripe.String(string(stripe.CurrencyUSD)),
		Description: stripe.String(description),
	}
	params.Context = ctx
	params.SetIdempotencyKey(idempotencyKey)
	_, err := invoiceitem.New(params)
	return err
}

// CreateMarketSubscription subscribes customerID to the metered marketplace price (B20.2). The workspace is
// named as market_workspace_id — never workspace_id, which would make it the workspace's plan — and the
// idempotency key makes two first uses at once one subscription.
func (l *LiveStripe) CreateMarketSubscription(ctx context.Context, customerID, priceID, workspaceID string) (string, error) {
	params := &stripe.SubscriptionParams{
		Customer: stripe.String(customerID),
		Items:    []*stripe.SubscriptionItemsParams{{Price: stripe.String(priceID)}},
	}
	params.Context = ctx
	params.AddMetadata("market_workspace_id", workspaceID)
	params.SetIdempotencyKey("market-bill-" + workspaceID)
	sub, err := subscription.New(params)
	if err != nil {
		return "", err
	}
	return sub.ID, nil
}

// CreditMarketUse puts a NEGATIVE line of cents (a decimal: a use's price may be a fraction of a cent) on
// the customer's next invoice of the marketplace subscription (B20.4), idempotent on idempotencyKey.
func (l *LiveStripe) CreditMarketUse(ctx context.Context, customerID, subscriptionID string, cents float64, description, idempotencyKey string) (string, error) {
	params := &stripe.InvoiceItemParams{
		Customer:          stripe.String(customerID),
		Subscription:      stripe.String(subscriptionID),
		Currency:          stripe.String(string(stripe.CurrencyUSD)),
		UnitAmountDecimal: stripe.Float64(-cents),
		Quantity:          stripe.Int64(1),
		Description:       stripe.String(description),
	}
	params.Context = ctx
	params.SetIdempotencyKey(idempotencyKey)
	item, err := invoiceitem.New(params)
	if err != nil {
		return "", err
	}
	return item.ID, nil
}

// SendMeterEvent records one marketplace use of value µLXC for customerID (B20.2). Stripe keeps one event
// per identifier, so a retried use is billed once.
func (l *LiveStripe) SendMeterEvent(ctx context.Context, eventName, customerID, identifier string, value int64, at time.Time) error {
	params := &stripe.BillingMeterEventParams{
		EventName:  stripe.String(eventName),
		Identifier: stripe.String(identifier),
		Payload:    map[string]string{"stripe_customer_id": customerID, "value": strconv.FormatInt(value, 10)},
		Timestamp:  stripe.Int64(at.Unix()),
	}
	params.Context = ctx
	_, err := meterevent.New(params)
	return err
}
