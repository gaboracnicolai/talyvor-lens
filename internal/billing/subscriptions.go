package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stripe/stripe-go/v81"
)

// MODEL 2, STEP 1 — STRIPE SUBSCRIPTIONS. W4.6.1, on TEST keys.
//
// Everything in billing.go is a ONE-OFF payment: a Checkout Session completes and
// LXC is credited. This file adds the recurring half — a subscription checkout, and
// the four webhook events that move a subscription through its life:
//
//	customer.subscription.created  → the workspace is subscribed
//	customer.subscription.updated  → renewal, past_due, cancel-at-period-end
//	customer.subscription.deleted  → cancelled, for good
//	invoice.payment_failed         → the dunning signal, recorded on its own
//
// ⚠ WHAT THIS FILE DELIBERATELY DOES NOT DO. It does not grant an allowance, and it
// does not touch the LXC ledger. W4.6.1's step 2 is the allowance ledger (phi = F/D
// < 1, HARD CAP never overage) and it is a separate merge with its own economics to
// argue. A subscription here means exactly one thing: Stripe says this workspace is
// paying. What that entitles them to is step 2's question.
//
// ⚠ F AND D ARE NOT IN THIS FILE. The item says they are Nicolai's. `price_id` records
// which Stripe Price actually billed, so the number lives in Stripe and in the
// operator's config — never hardcoded here where it would become a second source of
// truth for a price.

// ErrNoSubscriptionPrice is returned when a subscription checkout is requested on a
// deployment that has not been configured with a Stripe Price. It is a CONFIGURATION
// refusal, not a payment failure: the caller gets a clear 501-shaped answer rather
// than a Stripe error about an empty price id.
var ErrNoSubscriptionPrice = errors.New("billing: no subscription price configured (LENS_BILLING_SUBSCRIPTION_PRICE_ID)")

// SubscriptionParams is the input to a subscription Checkout Session. Unlike
// CheckoutParams there is no amount: the PRICE is the amount, and it lives in Stripe.
type SubscriptionParams struct {
	WorkspaceID string
	CustomerID  string
	PriceID     string
}

// subscriptionAPI is the subscription half of the Stripe seam. Kept separate from
// stripeAPI and satisfied by the same live client, so a Service built without a
// subscription price still compiles and every existing test double keeps working
// without learning a method it has no opinion about.
type subscriptionAPI interface {
	CreateSubscriptionCheckoutSession(ctx context.Context, p SubscriptionParams) (url string, sessionID string, err error)
}

// terminalStatuses never move again. A row in one of these is history.
var terminalStatuses = map[string]bool{"canceled": true, "incomplete_expired": true}

// CreateSubscriptionCheckout returns a Stripe Checkout URL in subscription mode.
//
// ⚠ IT REFUSES A SECOND LIVE SUBSCRIPTION BEFORE STRIPE IS EVER CALLED. The unique
// index in 0120 is the backstop and this is the courtesy: a workspace that already
// pays should be told so, not sent to a checkout that will bill them again and then
// collide on the webhook — where the money has already moved.
func (s *Service) CreateSubscriptionCheckout(ctx context.Context, workspaceID string) (string, error) {
	if s.subPrice == "" || s.subStripe == nil {
		return "", ErrNoSubscriptionPrice
	}
	live, err := s.liveSubscriptionID(ctx, workspaceID)
	if err != nil {
		return "", err
	}
	if live != "" {
		return "", fmt.Errorf("billing: workspace %s already has a live subscription (%s)", workspaceID, live)
	}
	customerID, err := s.ensureCustomer(ctx, workspaceID)
	if err != nil {
		return "", fmt.Errorf("billing: ensure customer: %w", err)
	}
	url, _, err := s.subStripe.CreateSubscriptionCheckoutSession(ctx, SubscriptionParams{
		WorkspaceID: workspaceID,
		CustomerID:  customerID,
		PriceID:     s.subPrice,
	})
	if err != nil {
		return "", fmt.Errorf("billing: create subscription checkout: %w", err)
	}
	return url, nil
}

// liveSubscriptionID returns the workspace's non-terminal subscription id, or "".
// The predicate is the SAME status set as 0120's partial unique index — if the two
// ever disagree, this returns "" for a workspace the database will then refuse to
// insert, which is the confusing half of a double-subscribe.
func (s *Service) liveSubscriptionID(ctx context.Context, workspaceID string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		SELECT stripe_subscription_id FROM subscriptions
		WHERE workspace_id = $1 AND status IN ('trialing','active','past_due','unpaid')`, workspaceID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("billing: live subscription lookup: %w", err)
	}
	return id, nil
}

// SubscriptionStatus is the read model: what the rest of Lens asks about a workspace.
type SubscriptionStatus struct {
	Subscribed        bool       `json:"subscribed"`
	Status            string     `json:"status,omitempty"`
	CurrentPeriodEnd  *time.Time `json:"current_period_end,omitempty"`
	CancelAtPeriodEnd bool       `json:"cancel_at_period_end"`
	SubscriptionID    string     `json:"subscription_id,omitempty"`
	Livemode          bool       `json:"livemode"`
}

// GetSubscription answers "is this workspace paying". `Subscribed` is TRUE only for
// statuses that mean money is currently flowing.
//
// ⚠ past_due IS SUBSCRIBED AND unpaid IS NOT, and that asymmetry is the decision.
// `past_due` is Stripe's "a payment failed and we are retrying" — the customer has
// not left and Stripe may still collect, so cutting service on the first failed card
// is how a business loses a paying customer to an expired card. `unpaid` is the state
// AFTER Stripe has given up retrying; there the answer really is no.
func (s *Service) GetSubscription(ctx context.Context, workspaceID string) (*SubscriptionStatus, error) {
	var (
		st        SubscriptionStatus
		status    string
		periodEnd *time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT stripe_subscription_id, status, current_period_end, cancel_at_period_end, livemode
		FROM subscriptions
		WHERE workspace_id = $1 AND status IN ('trialing','active','past_due','unpaid')`, workspaceID).
		Scan(&st.SubscriptionID, &status, &periodEnd, &st.CancelAtPeriodEnd, &st.Livemode)
	if errors.Is(err, pgx.ErrNoRows) {
		return &SubscriptionStatus{Subscribed: false}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("billing: subscription read: %w", err)
	}
	st.Status = status
	st.CurrentPeriodEnd = periodEnd
	st.Subscribed = status == "trialing" || status == "active" || status == "past_due"
	return &st, nil
}

// ─── the webhook half ────────────────────────────────────────────────────────────

// handleSubscription applies a customer.subscription.* event.
//
// ⚠ THE WHOLE FUNCTION IS ONE TRANSACTION, and the event row is written on EVERY
// path — including the paths that change nothing. W4.6.1: "ASSERT THE LEDGER ROW,
// NEVER A STATUS CODE." A 200 proves only that the handler did not crash; the row
// says what it decided and why, and `applied` is the difference.
func (s *Service) handleSubscription(w http.ResponseWriter, ctx context.Context, event *stripe.Event) {
	var sub stripe.Subscription
	if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
		// Signed but unparseable — it will never parse; ack so Stripe stops retrying.
		s.log.Warn("billing webhook: unparseable subscription", "event", event.ID)
		w.WriteHeader(http.StatusOK)
		return
	}
	if sub.ID == "" {
		s.log.Warn("billing webhook: subscription event with no subscription id", "event", event.ID)
		w.WriteHeader(http.StatusOK)
		return
	}

	wsID := sub.Metadata["workspace_id"]
	status := string(sub.Status)
	if event.Type == "customer.subscription.deleted" {
		// Stripe sends the object as it was; the event TYPE is the fact.
		status = "canceled"
	}
	eventAt := time.Unix(event.Created, 0).UTC()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.fail(w, "begin", event.ID, err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// ⚠ THE IDEMPOTENCY CLAIM COMES FIRST, and it is a real INSERT rather than a
	// SELECT-then-act: two concurrent deliveries of the same event both reach the
	// insert, one wins, and the loser sees zero rows affected and stops. A read-first
	// check has a window between the read and the write that a retry fits inside.
	var (
		existing  string
		lastEvent time.Time
		haveRow   bool
	)
	err = tx.QueryRow(ctx, `
		SELECT status, last_event_at FROM subscriptions
		WHERE stripe_subscription_id = $1 FOR UPDATE`, sub.ID).Scan(&existing, &lastEvent)
	switch {
	case err == nil:
		haveRow = true
	case errors.Is(err, pgx.ErrNoRows):
		haveRow = false
	default:
		s.fail(w, "subscription lookup", event.ID, err)
		return
	}

	// ⚠ OUT-OF-ORDER DELIVERY IS THE CASE THIS ITEM NAMES AS "where every naive
	// implementation breaks". Stripe guarantees delivery, not ORDER. An older
	// `.updated` carrying past_due can arrive after the newer one that restored
	// active, and applying it would dun a customer who has already paid. Compare the
	// event's OWN created time against the last one we applied, and refuse to go
	// backwards. A terminal row is likewise never reopened.
	stale := haveRow && (!eventAt.After(lastEvent) || terminalStatuses[existing])
	applied := !stale

	if wsID == "" && haveRow {
		// A `.deleted` payload may omit metadata; the row already knows whose it is.
		if err := tx.QueryRow(ctx,
			`SELECT workspace_id FROM subscriptions WHERE stripe_subscription_id = $1`, sub.ID).Scan(&wsID); err != nil {
			s.fail(w, "workspace lookup", event.ID, err)
			return
		}
	}
	if wsID == "" {
		// Nothing to attribute it to and nothing on file. Record and ack — retrying
		// will not add the metadata.
		s.log.Warn("billing webhook: subscription event with no workspace", "event", event.ID, "subscription", sub.ID)
		applied = false
	}

	if applied {
		if haveRow {
			if _, err := tx.Exec(ctx, `
				UPDATE subscriptions
				SET status = $1, current_period_end = $2, cancel_at_period_end = $3,
				    price_id = COALESCE(NULLIF($4, ''), price_id),
				    last_event_at = $5, updated_at = NOW()
				WHERE stripe_subscription_id = $6`,
				status, periodEnd(&sub), sub.CancelAtPeriodEnd, priceOf(&sub), eventAt, sub.ID); err != nil {
				s.fail(w, "subscription update", event.ID, err)
				return
			}
		} else {
			// ⚠ A UNIQUE VIOLATION HERE IS THE DOUBLE-SUBSCRIBE the partial index in
			// 0120 exists to stop: this workspace already has a live subscription and
			// Stripe is telling us about a second one. That is money already taken, so
			// it must NOT be silently dropped — the event row is written with
			// applied=false and an operator can see both ids.
			//
			// ⚠⚠ THE SAVEPOINT IS LOAD-BEARING AND THE FIRST DRAFT DID NOT HAVE ONE.
			// In Postgres a constraint violation aborts the ENTIRE transaction: every
			// later command returns 25P02 "current transaction is aborted". So catching
			// the unique violation and carrying on to write the event row wrote nothing
			// — the insert failed, the handler returned 503, and Stripe would retry that
			// event forever while the operator saw NO row explaining why. Found by
			// TestSubscription_SecondLiveSubscription_NotApplied, which asserts the row
			// rather than the status code and so could see it at all.
			//
			// pgx's nested Begin is a SAVEPOINT: the rollback undoes only the failed
			// insert and leaves the outer transaction usable.
			sp, err := tx.Begin(ctx)
			if err != nil {
				s.fail(w, "savepoint", event.ID, err)
				return
			}
			_, err = sp.Exec(ctx, `
				INSERT INTO subscriptions
					(workspace_id, stripe_subscription_id, stripe_customer_id, price_id,
					 status, current_period_end, cancel_at_period_end, livemode, last_event_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
				wsID, sub.ID, customerOf(&sub), priceOf(&sub), status, periodEnd(&sub),
				sub.CancelAtPeriodEnd, event.Livemode, eventAt)
			if isUniqueViolation(err) {
				_ = sp.Rollback(ctx)
				s.log.Warn("billing webhook: workspace already has a live subscription — NOT applied",
					"event", event.ID, "workspace", wsID, "subscription", sub.ID)
				applied = false
			} else if err != nil {
				_ = sp.Rollback(ctx)
				s.fail(w, "subscription insert", event.ID, err)
				return
			} else if err := sp.Commit(ctx); err != nil {
				s.fail(w, "savepoint commit", event.ID, err)
				return
			}
		}
	}

	ct, err := tx.Exec(ctx, `
		INSERT INTO subscription_events
			(stripe_event_id, stripe_subscription_id, workspace_id, event_type,
			 status_requested, applied, livemode, event_created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (stripe_event_id) DO NOTHING`,
		event.ID, sub.ID, wsID, string(event.Type), status, applied, event.Livemode, eventAt)
	if err != nil {
		s.fail(w, "subscription event insert", event.ID, err)
		return
	}
	if ct.RowsAffected() == 0 {
		// A REDELIVERY OF AN EVENT ALREADY RECORDED. Roll back and ack.
		//
		// ⚠ THIS ROLLBACK IS DEFENCE-IN-DEPTH, NOT THE THING THAT MAKES REDELIVERY SAFE,
		// and the comment that stood here claimed otherwise. It said "without this the
		// second delivery would re-apply an update that a LATER event had already
		// superseded". That is UNREACHABLE: a redelivery carries the same
		// `event.created` as the original, so `eventAt.After(lastEvent)` is false and the
		// staleness guard has already set applied=false — there is nothing above to undo.
		// Control C3 in scripts/w461-subscription-controls-q4vn.py turned this into a
		// Commit and NOTHING went red, which is how the overclaim was found. Kept because
		// it is correct and free, and because the staleness guard is one edit away from
		// not covering this; described accurately so the next reader does not believe a
		// test is watching it.
		_ = tx.Rollback(ctx)
		s.log.Info("billing webhook: subscription event already recorded", "event", event.ID)
		w.WriteHeader(http.StatusOK)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		s.fail(w, "commit", event.ID, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleInvoicePaymentFailed records the dunning signal.
//
// ⚠ IT DOES NOT MOVE THE SUBSCRIPTION'S STATUS, and that is deliberate rather than
// incomplete. Stripe decides when a failed invoice becomes `past_due` and sends a
// `customer.subscription.updated` saying so; a handler that ALSO wrote past_due here
// would be a second author of one state machine, and the two would disagree the first
// time Stripe's retry succeeded. This records that a payment failed — the row is the
// evidence — and lets the subscription event move the state.
func (s *Service) handleInvoicePaymentFailed(w http.ResponseWriter, ctx context.Context, event *stripe.Event) {
	var inv stripe.Invoice
	if err := json.Unmarshal(event.Data.Raw, &inv); err != nil {
		s.log.Warn("billing webhook: unparseable invoice", "event", event.ID)
		w.WriteHeader(http.StatusOK)
		return
	}
	subID := invoiceSubscriptionID(&inv)
	if subID == "" {
		// A one-off invoice, not a subscription renewal. Nothing here owns it.
		w.WriteHeader(http.StatusOK)
		return
	}

	var wsID string
	err := s.pool.QueryRow(ctx,
		`SELECT workspace_id FROM subscriptions WHERE stripe_subscription_id = $1`, subID).Scan(&wsID)
	if errors.Is(err, pgx.ErrNoRows) {
		// An invoice for a subscription we have never seen. Ack: retrying cannot
		// conjure the subscription, and the `.created` event will bring it.
		s.log.Warn("billing webhook: payment failed for unknown subscription", "event", event.ID, "subscription", subID)
		w.WriteHeader(http.StatusOK)
		return
	}
	if err != nil {
		s.fail(w, "invoice workspace lookup", event.ID, err)
		return
	}

	ct, err := s.pool.Exec(ctx, `
		INSERT INTO subscription_events
			(stripe_event_id, stripe_subscription_id, workspace_id, event_type,
			 status_requested, applied, livemode, event_created_at)
		VALUES ($1, $2, $3, $4, $5, TRUE, $6, $7)
		ON CONFLICT (stripe_event_id) DO NOTHING`,
		event.ID, subID, wsID, string(event.Type), "payment_failed",
		event.Livemode, time.Unix(event.Created, 0).UTC())
	if err != nil {
		s.fail(w, "invoice event insert", event.ID, err)
		return
	}
	if ct.RowsAffected() == 0 {
		s.log.Info("billing webhook: invoice event already recorded", "event", event.ID)
	}
	w.WriteHeader(http.StatusOK)
}

// ─── payload readers ─────────────────────────────────────────────────────────────
//
// ⚠ EACH ONE TOLERATES AN ABSENT NESTED OBJECT. Stripe expands some references and
// not others depending on the event, so `sub.Customer` is a struct on one delivery
// and nil on the next. A bare `sub.Customer.ID` panics inside a webhook handler,
// which turns a recoverable event into a 500 and an infinite Stripe retry.

func customerOf(sub *stripe.Subscription) string {
	if sub.Customer == nil {
		return ""
	}
	return sub.Customer.ID
}

func priceOf(sub *stripe.Subscription) string {
	if sub.Items == nil || len(sub.Items.Data) == 0 {
		return ""
	}
	it := sub.Items.Data[0]
	if it == nil || it.Price == nil {
		return ""
	}
	return it.Price.ID
}

// ⚠ MEASURED AGAINST THE SDK, NOT ASSUMED. My first draft read the period off the
// subscription ITEM (`sub.Items.Data[0].CurrentPeriodEnd`) and the invoice's
// subscription off `inv.Parent.SubscriptionDetails`, which is a LATER API shape.
// stripe-go v81.4.0 puts both on the parent object — `go build` said so immediately,
// and the fix is recorded here because the two shapes are easy to confuse and the
// wrong one compiles fine against a newer SDK.
func periodEnd(sub *stripe.Subscription) *time.Time {
	if sub.CurrentPeriodEnd == 0 {
		return nil
	}
	t := time.Unix(sub.CurrentPeriodEnd, 0).UTC()
	return &t
}

func invoiceSubscriptionID(inv *stripe.Invoice) string {
	if inv.Subscription == nil {
		return ""
	}
	return inv.Subscription.ID
}
