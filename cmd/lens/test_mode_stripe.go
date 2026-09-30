package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/talyvor/lens/internal/agentcard"
	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
)

// test_mode_stripe.go — B25.6: A TEST USER'S MARKETPLACE BILL, PAYOUTS AND AGENT CARDS STAY IN STRIPE TEST
// MODE, EVEN ONCE LENS'S KEY IS LIVE.
//
// B25.2 gave test (synthetic) workspaces a billing Service of their own on the Stripe test-mode key for
// top-ups and subscriptions. The rest of a test user's Stripe goes the same way: stripeByKind picks, per
// workspace, the marketplace bill its paid uses are metered onto and refunded on, the Connect client its
// seller account is made and paid with, and the Issuing client its agents' cards come from. A real
// workspace keeps what it had, on LENS_STRIPE_SECRET_KEY.

// stripeSide is one kind of workspace's Stripe. A nil part is one that kind has not got.
type stripeSide struct {
	bill    *billing.Service // the marketplace bill: a Service WithMarketBill
	connect market.ConnectStripe
	cards   agentcard.Issuer
}

// stripeByKind is a workspace's Stripe by its kind: test (synthetic) workspaces' on the test-mode key, every
// other one's on the main key.
type stripeByKind struct {
	isTest     func(wsID string) bool
	live, test stripeSide
	// the test-mode variables a test workspace would need for a part it has not got.
	billUnset, connectUnset, cardsUnset string
}

// errNoTestMode refuses a test workspace a part of Stripe it has not got in test mode, naming what to set. It
// is answered 403, as B25.2's checkout refusal is, so that the variable reaches the caller.
var errNoTestMode = errors.New("a test workspace uses Stripe only in test mode")

// newStripeByKind builds it from the test workspaces' own test-mode side, test, whose parts are nil where
// the test-mode variables are not set. A part they have not got of their own is the main key's while that
// key is itself test mode, as it was before B25.6 — except the bill when they have a test-mode Service of
// their own, because the main Service's webhook then leaves their invoices to it (billing.ForTestWorkspaces).
// Once the main key is live they have none, and are refused, rather than use live money.
func newStripeByKind(isTest func(string) bool, mainKeyLive, ownTestService bool, live, test stripeSide) stripeByKind {
	const keys = "LENS_STRIPE_TEST_SECRET_KEY and LENS_STRIPE_TEST_WEBHOOK_SECRET"
	k := stripeByKind{isTest: isTest, live: live, test: test, billUnset: keys, connectUnset: keys, cardsUnset: "LENS_STRIPE_TEST_SECRET_KEY"}
	if ownTestService {
		k.billUnset = "LENS_STRIPE_TEST_MARKET_BILL_PRICE_ID"
	}
	if !mainKeyLive {
		if k.test.bill == nil && !ownTestService {
			k.test.bill = live.bill
		}
		if k.test.connect == nil {
			k.test.connect = live.connect
		}
		if k.test.cards == nil {
			k.test.cards = live.cards
		}
	}
	return k
}

func (k stripeByKind) side(wsID string) stripeSide {
	if k.isTest(wsID) {
		return k.test
	}
	return k.live
}

// hasBill reports whether any workspace has a marketplace bill.
func (k stripeByKind) hasBill() bool { return k.live.bill != nil || k.test.bill != nil }

// billFor is the marketplace bill wsID's paid uses go on. nil with no error: there is none here for anyone.
func (k stripeByKind) billFor(wsID string) (*billing.Service, error) {
	if b := k.side(wsID).bill; b != nil {
		return b, nil
	}
	if k.isTest(wsID) && k.live.bill != nil {
		return nil, fmt.Errorf("%w: set %s for its marketplace bill", errNoTestMode, k.billUnset)
	}
	return nil, nil
}

// meterFor is the bill wsID's paid uses are metered onto, or nil — then a paid listing is refused
// (market.ErrNoBill) before it runs, so no use waits on a bill it can never go on.
func (k stripeByKind) meterFor(wsID string) market.Meter {
	if b, _ := k.billFor(wsID); b != nil {
		return b
	}
	return nil
}

// MeterMarketUse puts a use on its buyer's bill: market.Meter, for the pass that bills pending uses.
func (k stripeByKind) MeterMarketUse(ctx context.Context, buyerWorkspaceID, useID string, ulxc int64, at time.Time) error {
	b, err := k.billFor(buyerWorkspaceID)
	if b == nil {
		return errors.Join(market.ErrNoBill, err)
	}
	return b.MeterMarketUse(ctx, buyerWorkspaceID, useID, ulxc, at)
}

// CreditMarketRefund credits a refunded use on its buyer's bill: market.Refunder (B20.4).
func (k stripeByKind) CreditMarketRefund(ctx context.Context, buyerWorkspaceID, useID string, ulxc int64, description string) (string, error) {
	b, err := k.billFor(buyerWorkspaceID)
	if b == nil {
		return "", errors.Join(market.ErrNoBill, err)
	}
	return b.CreditMarketRefund(ctx, buyerWorkspaceID, useID, ulxc, description)
}

// connectFor is the Connect client wsID's seller account is made and paid with, or nil and why not.
func (k stripeByKind) connectFor(wsID string) (market.ConnectStripe, error) {
	if c := k.side(wsID).connect; c != nil {
		return c, nil
	}
	if k.isTest(wsID) && k.live.connect != nil {
		return nil, fmt.Errorf("%w: set %s for its payouts", errNoTestMode, k.connectUnset)
	}
	return nil, errPayoutsOff
}

// IssueCard issues an agent's card with its workspace's Issuing client: agentcard.Issuer.
func (k stripeByKind) IssueCard(ctx context.Context, workspaceID, agentID, agentName string, holder agentcard.Cardholder) (economy.AgentCard, error) {
	c := k.side(workspaceID).cards
	if c == nil {
		return economy.AgentCard{}, fmt.Errorf("%w: set %s for its agents' cards", errNoTestMode, k.cardsUnset)
	}
	return c.IssueCard(ctx, workspaceID, agentID, agentName, holder)
}

// payOut is one payout run: test sellers with the test-mode key, real sellers with the main key.
func (k stripeByKind) payOut(ctx context.Context, store *market.Store, now time.Time) (int, error) {
	paid, errs := 0, []error{}
	for _, run := range []struct {
		test    bool
		connect market.ConnectStripe
	}{{true, k.test.connect}, {false, k.live.connect}} {
		if run.connect == nil {
			continue
		}
		n, err := store.PayOutSellers(ctx, run.connect, run.test, now)
		paid += n
		if err != nil {
			errs = append(errs, err)
		}
	}
	return paid, errors.Join(errs...)
}
