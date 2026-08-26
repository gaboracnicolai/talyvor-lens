package billing

import (
	"context"
	"strconv"

	stripe "github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/checkout/session"
	"github.com/stripe/stripe-go/v81/customer"
	"github.com/stripe/stripe-go/v81/paymentintent"
)

// LiveStripe is the production stripeAPI implementation. It is NOT exercised by
// unit tests (those inject a fake); the webhook/credit money path is what the
// real-PG tests cover. The secret key is set on the process-global stripe.Key.
type LiveStripe struct {
	successURL string
	cancelURL  string
}

// NewLiveStripe configures the Stripe SDK with the secret key and the
// success/cancel redirect URLs Checkout requires.
func NewLiveStripe(secretKey, successURL, cancelURL string) *LiveStripe {
	stripe.Key = secretKey
	return &LiveStripe{successURL: successURL, cancelURL: cancelURL}
}

// CreateCustomer creates a Stripe customer tagged with the workspace id.
func (l *LiveStripe) CreateCustomer(ctx context.Context, workspaceID string) (string, error) {
	params := &stripe.CustomerParams{}
	params.Context = ctx
	params.AddMetadata("workspace_id", workspaceID)
	c, err := customer.New(params)
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
	}
	params.Context = ctx
	params.AddMetadata("workspace_id", p.WorkspaceID)
	params.AddMetadata("lxc_amount", strconv.FormatInt(p.LXCAmount, 10)) // µLXC (SEC-2)
	params.AddMetadata("usd_cents", strconv.FormatInt(p.USDCents, 10))

	sess, err := session.New(params)
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
	pi, err := paymentintent.Get(paymentIntentID, params)
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
	sess, err := session.New(params)
	if err != nil {
		return "", "", err
	}
	return sess.URL, sess.ID, nil
}
