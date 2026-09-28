package market

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/billing"
)

// payout.go — B20.5: SELLERS ARE PAID IN MONEY, THROUGH STRIPE CONNECT.
//
//   - A seller connects a Stripe Express account through Stripe's onboarding link; Stripe collects their
//     identity, bank and tax details, and Lens records what Stripe says of the account (market_sellers).
//   - A seller's AVAILABLE balance is every share they earned whose 14-day holdback has passed and that no
//     refund reversed, less every payout. Once a month, a connected seller whose available balance has
//     reached US$25 is paid it: one market_payouts row, then one Stripe transfer of it less Stripe's fees
//     at cost — $2 for an account paid in a month, and 0.25% + $0.25 for the payout — every fee shown.
//   - A seller may instead take their available balance as Talyvor credits, 1:1, at any time: one
//     market_payouts row and one lxc_ledger row, committed together.
//   - A buyer's refund or chargeback reverses the earnings of the uses it paid for (a market_refunds row
//     each). Inside the holdback, the earning never becomes available; after it, the balance falls by it,
//     and a balance that falls below zero is owed and recovered from the seller's future earnings.

const (
	// PayoutMinimumUSDMicros: a seller is paid in money once their available balance reaches US$25.
	PayoutMinimumUSDMicros = 25_000_000
	// Stripe's Connect fees, deducted at cost: $2 per account paid in a month, and 0.25% + $0.25 per payout.
	stripeAccountFeeCents   = 200
	stripePayoutFixedCents  = 25
	stripePayoutBasisPoints = 25

	PayoutStripe  = "stripe"
	PayoutCredits = "credits"

	usdMicrosPerCent = 10_000
)

// ErrNotConnected: the seller has no connected Stripe account yet.
var ErrNotConnected = errors.New("market: connect a Stripe account to be paid")

// ErrNothingAvailable: no earnings are past their holdback and unpaid.
var ErrNothingAvailable = errors.New("market: no earnings are available yet — they become available 14 days after the buyer's payment clears")

// ConnectStripe is Stripe Connect as the payouts use it. *billing.LiveStripe satisfies it.
type ConnectStripe interface {
	CreateConnectedAccount(ctx context.Context, workspaceID, country string) (billing.ConnectAccount, error)
	OnboardingLink(ctx context.Context, accountID, refreshURL, returnURL string) (string, error)
	ConnectedAccount(ctx context.Context, accountID string) (billing.ConnectAccount, error)
	TransferToSeller(ctx context.Context, accountID string, cents int64, payoutID, workspaceID string) (transferID string, err error)
}

// Crediter writes LXC credits on the caller's transaction. *economy.DualTokenStore satisfies it.
type Crediter interface {
	CreditLXCTx(ctx context.Context, tx pgx.Tx, workspaceID string, lxcAmount int64, reason string, metadata map[string]interface{}) (int64, error)
}

// PayoutFees splits a payout of grossCents into Stripe's fees and what the seller receives. The payout fee
// is 0.25% of what is paid out plus $0.25, so net is solved for: net + 0.25%·net = gross − $2 − $0.25,
// rounded down to the cent (the fee carries the fraction).
func PayoutFees(grossCents int64) (accountFeeCents, payoutFeeCents, netCents int64) {
	netCents = (grossCents - stripeAccountFeeCents - stripePayoutFixedCents) * 10_000 / (10_000 + stripePayoutBasisPoints)
	if netCents < 0 {
		netCents = 0
	}
	return stripeAccountFeeCents, grossCents - stripeAccountFeeCents - netCents, netCents
}

// Payout is one market_payouts row.
type Payout struct {
	ID                  string     `json:"id"`
	Method              string     `json:"method"`
	Month               string     `json:"month"`
	GrossUSDMicros      int64      `json:"gross_usd_micros"`
	AccountFeeUSDMicros int64      `json:"account_fee_usd_micros"`
	PayoutFeeUSDMicros  int64      `json:"payout_fee_usd_micros"`
	NetUSDMicros        int64      `json:"net_usd_micros"`
	CreditsULXC         int64      `json:"credits_ulxc,omitempty"`
	StripeTransferID    string     `json:"stripe_transfer_id,omitempty"`
	PaidAt              *time.Time `json:"paid_at,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
}

// PayoutQuote is what paying the available balance out now would come to, fees shown.
type PayoutQuote struct {
	GrossUSDMicros      int64 `json:"gross_usd_micros"`
	AccountFeeUSDMicros int64 `json:"account_fee_usd_micros"`
	PayoutFeeUSDMicros  int64 `json:"payout_fee_usd_micros"`
	NetUSDMicros        int64 `json:"net_usd_micros"`
}

// Payouts is a seller's payout page.
type Payouts struct {
	Account             *billing.ConnectAccount `json:"account"` // nil: not connected
	InHoldbackUSDMicros int64                   `json:"in_holdback_usd_micros"`
	AvailableUSDMicros  int64                   `json:"available_usd_micros"`
	OwedUSDMicros       int64                   `json:"owed_usd_micros"` // reversed after being paid: recovered from future earnings
	PaidOutUSDMicros    int64                   `json:"paid_out_usd_micros"`
	MinimumUSDMicros    int64                   `json:"minimum_usd_micros"`
	PaidThisMonth       bool                    `json:"paid_this_month"`
	Quote               PayoutQuote             `json:"quote"` // a money payout of the available balance
	Payouts             []Payout                `json:"payouts"`
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// sellerBalance reads a seller's earnings past the holdback and still inside it (neither counting what a
// refund reversed), and everything paid out.
func sellerBalance(ctx context.Context, q queryRower, workspaceID string, now time.Time) (released, inHoldback, paid int64, err error) {
	err = q.QueryRow(ctx, `SELECT
		COALESCE((SELECT sum(e.share_usd_micros) FROM market_earnings e
			WHERE e.seller_workspace_id = $1 AND e.payable_at <= $2 AND NOT EXISTS (SELECT 1 FROM market_refunds r WHERE r.use_id = e.use_id)), 0)::bigint,
		COALESCE((SELECT sum(e.share_usd_micros) FROM market_earnings e
			WHERE e.seller_workspace_id = $1 AND e.payable_at > $2 AND NOT EXISTS (SELECT 1 FROM market_refunds r WHERE r.use_id = e.use_id)), 0)::bigint,
		COALESCE((SELECT sum(gross_usd_micros) FROM market_payouts WHERE workspace_id = $1), 0)::bigint`,
		workspaceID, now).Scan(&released, &inHoldback, &paid)
	return released, inHoldback, paid, err
}

func lockSeller(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('market_seller:' || $1, 0))`, workspaceID)
	return err
}

func monthOf(t time.Time) string { return t.UTC().Format("2006-01") }

// ConnectSeller gives the seller a link to Stripe's onboarding for their connected account, creating the
// account the first time. country (ISO 3166-1 alpha-2, "" for the platform's) is fixed once it exists.
func (s *Store) ConnectSeller(ctx context.Context, api ConnectStripe, workspaceID, country, refreshURL, returnURL string) (string, billing.ConnectAccount, error) {
	country = strings.ToUpper(strings.TrimSpace(country))
	if country != "" && len(country) != 2 {
		return "", billing.ConnectAccount{}, invalid("country must be a two-letter code, such as GB or US")
	}
	a, err := s.sellerAccount(ctx, workspaceID)
	if errors.Is(err, ErrNotConnected) {
		created, cerr := api.CreateConnectedAccount(ctx, workspaceID, country)
		if cerr != nil {
			return "", a, fmt.Errorf("market: create the seller's Stripe account: %w", cerr)
		}
		if _, err := s.pool.Exec(ctx, `INSERT INTO market_sellers (workspace_id, stripe_account_id, country, details_submitted, payouts_enabled, currently_due, disabled_reason, checked_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, now()) ON CONFLICT (workspace_id) DO NOTHING`,
			workspaceID, created.ID, created.Country, created.DetailsSubmitted, created.PayoutsEnabled, created.CurrentlyDue, created.DisabledReason); err != nil {
			return "", a, fmt.Errorf("market: record the seller's Stripe account: %w", err)
		}
		a, err = s.sellerAccount(ctx, workspaceID)
	}
	if err != nil {
		return "", a, err
	}
	url, err := api.OnboardingLink(ctx, a.ID, refreshURL, returnURL)
	if err != nil {
		return "", a, fmt.Errorf("market: Stripe onboarding link: %w", err)
	}
	return url, a, nil
}

func (s *Store) sellerAccount(ctx context.Context, workspaceID string) (billing.ConnectAccount, error) {
	var a billing.ConnectAccount
	err := s.pool.QueryRow(ctx, `SELECT stripe_account_id, country, details_submitted, payouts_enabled, currently_due, disabled_reason
		FROM market_sellers WHERE workspace_id = $1`, workspaceID).
		Scan(&a.ID, &a.Country, &a.DetailsSubmitted, &a.PayoutsEnabled, &a.CurrentlyDue, &a.DisabledReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNotConnected
	}
	if err != nil {
		return a, fmt.Errorf("market: seller account: %w", err)
	}
	return a, nil
}

// RecordSellerAccount records what Stripe says of a seller's connected account.
func (s *Store) RecordSellerAccount(ctx context.Context, a billing.ConnectAccount) error {
	if a.CurrentlyDue == nil {
		a.CurrentlyDue = []string{}
	}
	_, err := s.pool.Exec(ctx, `UPDATE market_sellers SET details_submitted = $2, payouts_enabled = $3, currently_due = $4,
		disabled_reason = $5, checked_at = now() WHERE stripe_account_id = $1`,
		a.ID, a.DetailsSubmitted, a.PayoutsEnabled, a.CurrentlyDue, a.DisabledReason)
	if err != nil {
		return fmt.Errorf("market: record seller account %s: %w", a.ID, err)
	}
	return nil
}

// SellerPayouts reads a seller's payout page. A connected account Stripe has not yet enabled for payouts
// is asked about first (api nil: what was last recorded).
func (s *Store) SellerPayouts(ctx context.Context, api ConnectStripe, workspaceID string, now time.Time) (Payouts, error) {
	p := Payouts{MinimumUSDMicros: PayoutMinimumUSDMicros, Payouts: []Payout{}}
	a, err := s.sellerAccount(ctx, workspaceID)
	switch {
	case errors.Is(err, ErrNotConnected):
	case err != nil:
		return p, err
	default:
		if !a.PayoutsEnabled && api != nil {
			if fresh, err := api.ConnectedAccount(ctx, a.ID); err == nil {
				if err := s.RecordSellerAccount(ctx, fresh); err != nil {
					return p, err
				}
				fresh.Country = a.Country
				a = fresh
			}
		}
		p.Account = &a
	}
	released, inHoldback, paid, err := sellerBalance(ctx, s.pool, workspaceID, now)
	if err != nil {
		return p, fmt.Errorf("market: seller balance: %w", err)
	}
	p.InHoldbackUSDMicros, p.PaidOutUSDMicros = inHoldback, paid
	p.AvailableUSDMicros, p.OwedUSDMicros = max(released-paid, 0), max(paid-released, 0)
	gross := p.AvailableUSDMicros / usdMicrosPerCent
	if accountFee, payoutFee, net := PayoutFees(gross); net > 0 {
		p.Quote = PayoutQuote{gross * usdMicrosPerCent, accountFee * usdMicrosPerCent, payoutFee * usdMicrosPerCent, net * usdMicrosPerCent}
	}
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM market_payouts WHERE workspace_id = $1 AND method = 'stripe' AND month = $2)`,
		workspaceID, monthOf(now)).Scan(&p.PaidThisMonth); err != nil {
		return p, fmt.Errorf("market: payouts: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT id, method, month, gross_usd_micros, account_fee_usd_micros, payout_fee_usd_micros, net_usd_micros,
		       credits_ulxc, COALESCE(stripe_transfer_id, ''), paid_at, last_error, created_at
		FROM market_payouts WHERE workspace_id = $1 ORDER BY created_at DESC, id LIMIT 100`, workspaceID)
	if err != nil {
		return p, fmt.Errorf("market: payouts: %w", err)
	}
	p.Payouts, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Payout, error) {
		var x Payout
		return x, row.Scan(&x.ID, &x.Method, &x.Month, &x.GrossUSDMicros, &x.AccountFeeUSDMicros, &x.PayoutFeeUSDMicros, &x.NetUSDMicros,
			&x.CreditsULXC, &x.StripeTransferID, &x.PaidAt, &x.LastError, &x.CreatedAt)
	})
	if err != nil {
		return p, fmt.Errorf("market: payouts: %w", err)
	}
	return p, nil
}

// TakeAsCredits pays the seller's whole available balance as Talyvor credits, 1:1: one market_payouts row
// and one lxc_ledger row, in one transaction.
func (s *Store) TakeAsCredits(ctx context.Context, crediter Crediter, workspaceID string, now time.Time) (Payout, error) {
	var p Payout
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockSeller(ctx, tx, workspaceID); err != nil {
			return err
		}
		released, _, paid, err := sellerBalance(ctx, tx, workspaceID, now)
		if err != nil {
			return err
		}
		if released-paid <= 0 {
			return ErrNothingAvailable
		}
		gross := released - paid
		p = Payout{ID: "mpo_" + uuid.NewString(), Method: PayoutCredits, Month: monthOf(now), GrossUSDMicros: gross, NetUSDMicros: gross,
			CreditsULXC: gross * ulxcPerUSDMicro}
		if err := tx.QueryRow(ctx, `INSERT INTO market_payouts (id, workspace_id, method, month, gross_usd_micros, net_usd_micros, credits_ulxc, paid_at, created_at)
			VALUES ($1, $2, 'credits', $3, $4, $4, $5, $6, $6) RETURNING paid_at, created_at`,
			p.ID, workspaceID, p.Month, gross, p.CreditsULXC, now).Scan(&p.PaidAt, &p.CreatedAt); err != nil {
			return err
		}
		_, err = crediter.CreditLXCTx(ctx, tx, workspaceID, p.CreditsULXC, "marketplace earnings taken as credits",
			map[string]interface{}{"market_payout_id": p.ID, "usd_micros": gross})
		return err
	})
	if err != nil && !errors.Is(err, ErrNothingAvailable) {
		err = fmt.Errorf("market: take earnings as credits: %w", err)
	}
	return p, err
}

// PayOut is the monthly payout run: it pays every connected seller not yet paid in money this month whose
// available balance has reached the minimum and whose account Stripe — asked now — has enabled for
// payouts, and it retries every transfer Stripe has not yet accepted. It answers how many transfers Stripe
// accepted.
func (s *Store) PayOut(ctx context.Context, api ConnectStripe, now time.Time) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT workspace_id, stripe_account_id FROM market_sellers s
		WHERE NOT EXISTS (SELECT 1 FROM market_payouts p WHERE p.workspace_id = s.workspace_id AND p.method = 'stripe' AND p.month = $1)
		ORDER BY workspace_id`, monthOf(now))
	if err != nil {
		return 0, fmt.Errorf("market: sellers to pay: %w", err)
	}
	type seller struct{ ws, account string }
	sellers, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (seller, error) {
		var x seller
		return x, row.Scan(&x.ws, &x.account)
	})
	if err != nil {
		return 0, fmt.Errorf("market: sellers to pay: %w", err)
	}
	var askErr error
	for _, x := range sellers {
		released, _, paid, err := sellerBalance(ctx, s.pool, x.ws, now)
		if err != nil {
			return 0, fmt.Errorf("market: payout for %s: %w", x.ws, err)
		}
		if released-paid < PayoutMinimumUSDMicros {
			continue
		}
		account, err := api.ConnectedAccount(ctx, x.account)
		if err != nil {
			if askErr == nil {
				askErr = fmt.Errorf("market: payout for %s: ask Stripe about %s: %w", x.ws, x.account, err)
			}
			continue // the next pass asks again
		}
		if err := s.RecordSellerAccount(ctx, account); err != nil {
			return 0, err
		}
		if !account.PayoutsEnabled {
			continue
		}
		err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			if err := lockSeller(ctx, tx, x.ws); err != nil {
				return err
			}
			released, _, paid, err := sellerBalance(ctx, tx, x.ws, now)
			if err != nil {
				return err
			}
			if released-paid < PayoutMinimumUSDMicros {
				return nil
			}
			gross := (released - paid) / usdMicrosPerCent
			accountFee, payoutFee, net := PayoutFees(gross)
			_, err = tx.Exec(ctx, `INSERT INTO market_payouts (id, workspace_id, method, month, gross_usd_micros, account_fee_usd_micros, payout_fee_usd_micros,
				net_usd_micros, stripe_account_id, created_at)
				VALUES ($1, $2, 'stripe', $3, $4, $5, $6, $7, $8, $9) ON CONFLICT (workspace_id, month) WHERE method = 'stripe' DO NOTHING`,
				"mpo_"+uuid.NewString(), x.ws, monthOf(now), gross*usdMicrosPerCent, accountFee*usdMicrosPerCent, payoutFee*usdMicrosPerCent,
				net*usdMicrosPerCent, x.account, now)
			return err
		})
		if err != nil {
			return 0, fmt.Errorf("market: payout for %s: %w", x.ws, err)
		}
	}
	n, err := s.transferUnpaid(ctx, api)
	if err == nil {
		err = askErr
	}
	return n, err
}

// transferUnpaid asks Stripe for the transfer of every payout it has not yet accepted.
func (s *Store) transferUnpaid(ctx context.Context, api ConnectStripe) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, workspace_id, stripe_account_id, net_usd_micros FROM market_payouts
		WHERE paid_at IS NULL ORDER BY created_at, id LIMIT 200`)
	if err != nil {
		return 0, fmt.Errorf("market: payouts to transfer: %w", err)
	}
	type due struct {
		id, ws, account string
		net             int64
	}
	todo, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (due, error) {
		var d due
		return d, row.Scan(&d.id, &d.ws, &d.account, &d.net)
	})
	if err != nil {
		return 0, fmt.Errorf("market: payouts to transfer: %w", err)
	}
	n := 0
	var firstErr error
	for _, d := range todo {
		transferID, err := api.TransferToSeller(ctx, d.account, d.net/usdMicrosPerCent, d.id, d.ws)
		if err != nil {
			if _, uerr := s.pool.Exec(ctx, `UPDATE market_payouts SET last_error = $2 WHERE id = $1 AND paid_at IS NULL`, d.id, err.Error()); uerr != nil {
				return n, fmt.Errorf("market: payout %s: %w", d.id, uerr)
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("market: transfer payout %s: %w", d.id, err)
			}
			continue
		}
		if _, err := s.pool.Exec(ctx, `UPDATE market_payouts SET stripe_transfer_id = $2, paid_at = now(), last_error = ''
			WHERE id = $1 AND paid_at IS NULL`, d.id, transferID); err != nil {
			return n, fmt.Errorf("market: payout %s: %w", d.id, err)
		}
		n++
	}
	return n, firstErr
}

// ReverseInvoice reverses the earnings of the uses a paid marketplace invoice carried, because its buyer
// was refunded ("buyer_refund") or charged back ("chargeback") by the Stripe object ref: a market_refunds
// row per use, oldest first, until the uses reversed for refunds and chargebacks of this invoice cover
// amountUSDMicros (every use when all). The buyer has their money back from Stripe, so nobody is credited;
// the seller's share comes out of their balance — inside the holdback before it was ever available, after
// it out of what is available, or owed from their future earnings.
func (s *Store) ReverseInvoice(ctx context.Context, invoiceID, cause, ref string, amountUSDMicros int64, all bool) (int, bool, error) {
	reason := map[string]string{"buyer_refund": "the buyer's payment was refunded", "chargeback": "the buyer's payment was charged back"}[cause]
	if reason == "" {
		return 0, false, fmt.Errorf("market: reverse invoice %s: unknown cause %q", invoiceID, cause)
	}
	rows, err := s.pool.Query(ctx, `SELECT u.id, r.use_id IS NOT NULL, COALESCE(r.cause, ''), u.price_ulxc FROM market_uses u
		LEFT JOIN market_refunds r ON r.use_id = u.id
		WHERE u.cleared_invoice_id = $1 ORDER BY u.used_at, u.id`, invoiceID)
	if err != nil {
		return 0, false, fmt.Errorf("market: uses of invoice %s: %w", invoiceID, err)
	}
	type use struct {
		id          string
		refunded    bool
		refundCause string
		priceULXC   int64
	}
	uses, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (use, error) {
		var u use
		return u, row.Scan(&u.id, &u.refunded, &u.refundCause, &u.priceULXC)
	})
	if err != nil {
		return 0, false, fmt.Errorf("market: uses of invoice %s: %w", invoiceID, err)
	}
	if len(uses) == 0 {
		return 0, false, nil
	}
	var covered int64
	for _, u := range uses {
		if u.refundCause == "buyer_refund" || u.refundCause == "chargeback" {
			covered += u.priceULXC / ulxcPerUSDMicro
		}
	}
	n := 0
	for _, u := range uses {
		if u.refunded {
			continue
		}
		if !all && covered >= amountUSDMicros {
			break
		}
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `INSERT INTO market_refunds (use_id, listing_id, buyer_workspace_id, seller_workspace_id, price_ulxc,
				gross_usd_micros, reversed_share_usd_micros, reason, cause, stripe_ref)
				SELECT u.id, u.listing_id, u.buyer_workspace_id, u.seller_workspace_id, u.price_ulxc, u.price_ulxc / $2,
				       COALESCE(e.share_usd_micros, 0), $3, $4, $5
				FROM market_uses u LEFT JOIN market_earnings e ON e.use_id = u.id
				WHERE u.id = $1
				ON CONFLICT (use_id) DO NOTHING`, u.id, ulxcPerUSDMicro, reason, cause, ref)
			n += int(tag.RowsAffected())
			return err
		})
		if err != nil {
			return n, true, fmt.Errorf("market: reverse use %s: %w", u.id, err)
		}
		covered += u.priceULXC / ulxcPerUSDMicro
	}
	return n, true, nil
}
