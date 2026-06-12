package billing

import (
	"context"
	"strconv"

	stripe "github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/checkout/session"
	"github.com/stripe/stripe-go/v81/customer"
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
	params.AddMetadata("lxc_amount", strconv.FormatFloat(p.LXCAmount, 'f', -1, 64))
	params.AddMetadata("usd_cents", strconv.FormatInt(p.USDCents, 10))

	sess, err := session.New(params)
	if err != nil {
		return "", "", err
	}
	return sess.URL, sess.ID, nil
}
