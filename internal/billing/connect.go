package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	stripe "github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/account"
	"github.com/stripe/stripe-go/v81/accountlink"
	"github.com/stripe/stripe-go/v81/charge"
	"github.com/stripe/stripe-go/v81/transfer"
)

// connect.go — B20.5: SELLERS ARE PAID IN MONEY, THROUGH STRIPE CONNECT.
//
// A seller's Express account is created by Lens and onboarded by Stripe (identity, bank and tax details
// are Stripe's to collect). Their payouts are transfers from the platform balance to that account; Stripe
// pays the account out to the seller's bank. What Stripe says of the account is asked of it (when the
// seller opens their payouts page, and before each payout) rather than read from account.updated, which
// Stripe sends only to a Connect endpoint with a signing secret of its own. The webhook reverses the
// earnings a buyer's refund (charge.refunded) or chargeback (charge.dispute.created) of a paid marketplace
// invoice paid for.

// ConnectAccount is what Stripe says of a seller's connected account.
type ConnectAccount struct {
	ID               string   `json:"stripe_account_id"`
	Country          string   `json:"country"`
	DetailsSubmitted bool     `json:"details_submitted"`
	PayoutsEnabled   bool     `json:"payouts_enabled"`
	CurrentlyDue     []string `json:"currently_due"`
	DisabledReason   string   `json:"disabled_reason,omitempty"`
}

func connectAccountOf(a *stripe.Account) ConnectAccount {
	c := ConnectAccount{ID: a.ID, Country: a.Country, DetailsSubmitted: a.DetailsSubmitted, PayoutsEnabled: a.PayoutsEnabled, CurrentlyDue: []string{}}
	if a.Requirements != nil {
		c.CurrentlyDue = append(c.CurrentlyDue, a.Requirements.CurrentlyDue...)
		c.DisabledReason = string(a.Requirements.DisabledReason)
	}
	return c
}

// CreateConnectedAccount creates a seller's Express account, asking for the transfers capability their
// payouts need. country "" lets Stripe use the platform's. One account per workspace, however often asked.
func (l *LiveStripe) CreateConnectedAccount(ctx context.Context, workspaceID, country string) (ConnectAccount, error) {
	params := &stripe.AccountParams{
		Type: stripe.String(string(stripe.AccountTypeExpress)),
		Capabilities: &stripe.AccountCapabilitiesParams{
			Transfers: &stripe.AccountCapabilitiesTransfersParams{Requested: stripe.Bool(true)},
		},
	}
	if country != "" {
		params.Country = stripe.String(country)
	}
	params.Context = ctx
	params.AddMetadata("market_workspace_id", workspaceID)
	params.SetIdempotencyKey("market-seller-" + workspaceID + "-" + country)
	a, err := account.New(params)
	if err != nil {
		return ConnectAccount{}, err
	}
	return connectAccountOf(a), nil
}

// OnboardingLink is a single-use link to Stripe's onboarding for the account: it returns to returnURL when
// done, and to refreshURL when the link has expired.
func (l *LiveStripe) OnboardingLink(ctx context.Context, accountID, refreshURL, returnURL string) (string, error) {
	params := &stripe.AccountLinkParams{
		Account:    stripe.String(accountID),
		RefreshURL: stripe.String(refreshURL),
		ReturnURL:  stripe.String(returnURL),
		Type:       stripe.String("account_onboarding"),
	}
	params.Context = ctx
	link, err := accountlink.New(params)
	if err != nil {
		return "", err
	}
	return link.URL, nil
}

// ConnectedAccount asks Stripe what it says of a seller's account now.
func (l *LiveStripe) ConnectedAccount(ctx context.Context, accountID string) (ConnectAccount, error) {
	params := &stripe.AccountParams{}
	params.Context = ctx
	a, err := account.GetByID(accountID, params)
	if err != nil {
		return ConnectAccount{}, err
	}
	return connectAccountOf(a), nil
}

// TransferToSeller moves cents (USD) from the platform balance to the seller's account for one payout. The
// payout's id is the transfer group and the idempotency key; a transfer already made for it — a retry
// whose first answer never arrived, even past the idempotency key's 24 hours — is returned, not repeated.
func (l *LiveStripe) TransferToSeller(ctx context.Context, accountID string, cents int64, payoutID, workspaceID string) (string, error) {
	list := &stripe.TransferListParams{TransferGroup: stripe.String(payoutID), Destination: stripe.String(accountID)}
	list.Context = ctx
	list.Limit = stripe.Int64(1)
	if it := transfer.List(list); it.Next() {
		return it.Transfer().ID, nil
	} else if err := it.Err(); err != nil {
		return "", err
	}
	params := &stripe.TransferParams{
		Amount:        stripe.Int64(cents),
		Currency:      stripe.String(string(stripe.CurrencyUSD)),
		Destination:   stripe.String(accountID),
		TransferGroup: stripe.String(payoutID),
		Description:   stripe.String("Talyvor marketplace earnings"),
	}
	params.Context = ctx
	params.AddMetadata("market_payout_id", payoutID)
	params.AddMetadata("market_workspace_id", workspaceID)
	params.SetIdempotencyKey("market-payout-" + payoutID)
	t, err := transfer.New(params)
	if err != nil {
		return "", err
	}
	return t.ID, nil
}

// ChargeInvoice names the invoice a charge paid, and the charge's amount in cents. Asked of Stripe at the
// API version stripe-go pins, which still carries charge.invoice whatever the webhook endpoint's version.
func (l *LiveStripe) ChargeInvoice(ctx context.Context, chargeID string) (string, int64, error) {
	params := &stripe.ChargeParams{}
	params.Context = ctx
	ch, err := charge.Get(chargeID, params)
	if err != nil {
		return "", 0, err
	}
	if ch.Invoice == nil {
		return "", ch.Amount, nil
	}
	return ch.Invoice.ID, ch.Amount, nil
}

// connectStripeAPI is the webhook's half of the Connect seam. *LiveStripe satisfies it.
type connectStripeAPI interface {
	ChargeInvoice(ctx context.Context, chargeID string) (invoiceID string, amountCents int64, err error)
}

// MarketPayouts is what the webhook tells the marketplace. *market.Store satisfies it.
type MarketPayouts interface {
	// ReverseInvoice reverses the earnings of the uses a paid marketplace invoice carried, up to
	// amountUSDMicros of their price (all of them when all), because its buyer was refunded (cause
	// "buyer_refund") or charged back ("chargeback") by ref. It answers how many it reversed, and false
	// when the invoice carried no marketplace use.
	ReverseInvoice(ctx context.Context, invoiceID, cause, ref string, amountUSDMicros int64, all bool) (reversed int, marketplace bool, err error)
}

// WithMarketPayouts turns seller payouts on: charge.refunded and charge.dispute.created reach payouts,
// asking api which invoice a charge paid when the event does not say.
func (s *Service) WithMarketPayouts(api connectStripeAPI, payouts MarketPayouts) *Service {
	s.connectStripe, s.marketPayouts = api, payouts
	return s
}

// idOf reads a Stripe reference that is either an id or an expanded object.
func idOf(raw json.RawMessage) string {
	var id string
	if json.Unmarshal(raw, &id) == nil {
		return id
	}
	var obj struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &obj)
	return obj.ID
}

// reverseMarketCharge reverses the marketplace earnings a refunded charge paid for. It answers false when
// it has already answered the webhook (an error Stripe should retry).
func (s *Service) reverseMarketCharge(w http.ResponseWriter, ctx context.Context, event *stripe.Event) bool {
	if s.marketPayouts == nil {
		return true
	}
	var ch struct {
		ID             string          `json:"id"`
		Amount         int64           `json:"amount"`
		AmountRefunded int64           `json:"amount_refunded"`
		Refunded       bool            `json:"refunded"`
		Invoice        json.RawMessage `json:"invoice"`
	}
	if err := json.Unmarshal(event.Data.Raw, &ch); err != nil || ch.ID == "" {
		return true // handleRefund acks what it cannot parse
	}
	invoiceID := idOf(ch.Invoice)
	if invoiceID == "" && s.connectStripe != nil {
		var err error
		if invoiceID, _, err = s.connectStripe.ChargeInvoice(ctx, ch.ID); err != nil {
			s.fail(w, "refunded charge's invoice", event.ID, err)
			return false
		}
	}
	if invoiceID == "" {
		return true
	}
	// As handleRefund reads it: an event that does not say how much was refunded refunded all of it.
	all := ch.Refunded || ch.AmountRefunded == 0 || (ch.Amount > 0 && ch.AmountRefunded >= ch.Amount)
	n, market, err := s.marketPayouts.ReverseInvoice(ctx, invoiceID, "buyer_refund", ch.ID, ch.AmountRefunded*10_000, all)
	if err != nil {
		s.fail(w, "market refund reversal", event.ID, err)
		return false
	}
	if market {
		s.log.Info("billing webhook: a marketplace invoice was refunded", "invoice", invoiceID, "charge", ch.ID, "uses_reversed", n)
	}
	return true
}

// handleDispute reverses the marketplace earnings a charged-back payment paid for.
func (s *Service) handleDispute(w http.ResponseWriter, ctx context.Context, event *stripe.Event) {
	if s.marketPayouts == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	var d struct {
		ID     string          `json:"id"`
		Amount int64           `json:"amount"`
		Charge json.RawMessage `json:"charge"`
	}
	if err := json.Unmarshal(event.Data.Raw, &d); err != nil || d.ID == "" {
		s.log.Warn("billing webhook: unparseable dispute", "event", event.ID)
		w.WriteHeader(http.StatusOK)
		return
	}
	chargeID := idOf(d.Charge)
	if chargeID == "" || s.connectStripe == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	invoiceID, chargeCents, err := s.connectStripe.ChargeInvoice(ctx, chargeID)
	if err != nil {
		s.fail(w, "disputed charge's invoice", event.ID, err)
		return
	}
	if invoiceID == "" {
		w.WriteHeader(http.StatusOK) // not an invoice's payment: not a marketplace bill
		return
	}
	n, market, err := s.marketPayouts.ReverseInvoice(ctx, invoiceID, "chargeback", d.ID, d.Amount*10_000, d.Amount >= chargeCents)
	if err != nil {
		s.fail(w, "market chargeback reversal", event.ID, err)
		return
	}
	if market {
		s.log.Warn("billing webhook: a marketplace invoice was charged back", "invoice", invoiceID, "dispute", d.ID,
			"amount_cents", strconv.FormatInt(d.Amount, 10), "uses_reversed", n)
	}
	w.WriteHeader(http.StatusOK)
}
