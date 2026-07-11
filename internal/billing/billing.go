// Package billing owns ALL Stripe interaction for U18b: Checkout Session
// creation, webhook signature verification, and event handling that credits the
// fiat-pegged LXC usage credit. It calls economy.DualTokenStore.CreditLXCTx for
// the credit and writes its OWN tables (lxc_purchases, billing_customers); it
// never writes the LXC ledger (lxc_ledger/lxc_balances) directly.
//
// Billing is FIAT, independent of the U3 economy master switch. Money is integer
// USD cents; LXC is ALWAYS recomputed server-side from cents at the fixed peg
// (economy.LXCUSDValue), never trusted from Stripe session metadata.
package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	stripe "github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/webhook"

	"github.com/talyvor/lens/internal/economy"
)

// maxWebhookBody caps the raw webhook body (Stripe events are small; this is a
// DoS guard). The full body is still needed to verify the signature.
const maxWebhookBody = 1 << 20 // 1 MiB

// allowedTopUps — the server-side top-up sizes in USD cents ($10 / $50 / $100).
//
// ADDITIVE-ONLY while any checkout session may still be in flight: async payment
// methods (e.g. bank debits) can settle DAYS after session creation, and the
// webhook re-checks this list, so REMOVING a value would mark a legitimately-PAID
// purchase anomalous (charged, not credited). Only ever append sizes; never
// remove one until you are certain no session created with it can still settle.
var allowedTopUps = []int64{1000, 5000, 10000}

// ErrAmountNotAllowed is returned by CreateCheckout for an off-allow-list amount.
var ErrAmountNotAllowed = errors.New("billing: usd_cents is not an allowed top-up size")

// lxcCrediter is the LXC-credit surface billing needs — satisfied by
// *economy.DualTokenStore. Billing passes its OWN tx so the idempotency claim and
// the credit commit atomically.
type lxcCrediter interface {
	CreditLXCTx(ctx context.Context, tx pgx.Tx, workspaceID string, lxcAmount int64, reason string, metadata map[string]interface{}) (int64, error)
}

// stripeAPI abstracts the live Stripe calls (customer + checkout session) so the
// HTTP handlers test offline. Webhook verification uses the real
// webhook.ConstructEvent (pure crypto), not this interface.
type stripeAPI interface {
	CreateCustomer(ctx context.Context, workspaceID string) (customerID string, err error)
	CreateCheckoutSession(ctx context.Context, p CheckoutParams) (url string, sessionID string, err error)
	// CardFingerprint returns the stable per-card fingerprint for a payment
	// intent (Stripe's card.fingerprint), "" when none is available. U6 PR2
	// owner-linkage; called best-effort AFTER the credit commits.
	CardFingerprint(ctx context.Context, paymentIntentID string) (fingerprint string, err error)
}

// CheckoutParams is the price-true input to a Checkout Session: cents is
// authoritative, lxc is the peg recomputation, both flow into session metadata.
type CheckoutParams struct {
	WorkspaceID string
	CustomerID  string
	USDCents    int64
	LXCAmount   int64 // µLXC (SEC-2)
}

// Service is the billing core. Construct with New.
type Service struct {
	pool          *pgxpool.Pool
	credits       lxcCrediter
	stripe        stripeAPI
	webhookSecret string
	allowList     map[int64]struct{}
	wsExists      func(ctx context.Context, workspaceID string) (bool, error)
	log           *slog.Logger
}

// New builds a Service. wsExists defaults to a workspaces-table lookup.
func New(pool *pgxpool.Pool, credits lxcCrediter, sapi stripeAPI, webhookSecret string) *Service {
	al := make(map[int64]struct{}, len(allowedTopUps))
	for _, c := range allowedTopUps {
		al[c] = struct{}{}
	}
	return &Service{
		pool:          pool,
		credits:       credits,
		stripe:        sapi,
		webhookSecret: webhookSecret,
		allowList:     al,
		wsExists:      defaultWorkspaceExists(pool),
		log:           slog.Default(),
	}
}

func defaultWorkspaceExists(pool *pgxpool.Pool) func(context.Context, string) (bool, error) {
	return func(ctx context.Context, wsID string) (bool, error) {
		var one int
		err := pool.QueryRow(ctx, `SELECT 1 FROM workspaces WHERE id = $1`, wsID).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return true, nil
	}
}

// lxcForCents recomputes LXC from USD cents at the fixed peg — the SINGLE price
// truth, in µLXC (SEC-2). No LXC amount is hardcoded anywhere else in billing.
// A fiat purchase mints LXC to the payer, so it rounds DOWN (floor) — never
// over-credit. At the $0.10 peg the division is exact for integer cents anyway.
func lxcForCents(usdCents int64) int64 {
	return int64(math.Floor((float64(usdCents) / 100.0) / economy.LXCUSDValue * 1e6))
}

// AllowedTopUpCents returns the configured allow-list (for the checkout UI / docs).
func AllowedTopUpCents() []int64 { return append([]int64(nil), allowedTopUps...) }

// CreateCheckout validates the amount, recomputes LXC at the peg, ensures the
// Stripe customer mapping, and creates a Checkout Session. Returns the session
// URL. A disallowed amount yields ErrAmountNotAllowed (the caller maps it to 400).
func (s *Service) CreateCheckout(ctx context.Context, workspaceID string, usdCents int64) (string, error) {
	if _, ok := s.allowList[usdCents]; !ok {
		return "", ErrAmountNotAllowed
	}
	lxc := lxcForCents(usdCents)
	customerID, err := s.ensureCustomer(ctx, workspaceID)
	if err != nil {
		return "", fmt.Errorf("billing: ensure customer: %w", err)
	}
	url, _, err := s.stripe.CreateCheckoutSession(ctx, CheckoutParams{
		WorkspaceID: workspaceID,
		CustomerID:  customerID,
		USDCents:    usdCents,
		LXCAmount:   lxc,
	})
	if err != nil {
		return "", fmt.Errorf("billing: create checkout session: %w", err)
	}
	return url, nil
}

// ensureCustomer returns the workspace's Stripe customer id, creating + persisting
// the mapping on first use. Race note: two concurrent first-time checkouts may
// each create a Stripe customer; the DB INSERT ON CONFLICT keeps ONE mapping and
// the re-read returns the persisted (winning) id, so the credited path is always
// consistent — at worst an unused Stripe customer object is orphaned (no money).
func (s *Service) ensureCustomer(ctx context.Context, workspaceID string) (string, error) {
	var custID string
	err := s.pool.QueryRow(ctx,
		`SELECT stripe_customer_id FROM billing_customers WHERE workspace_id = $1`, workspaceID).Scan(&custID)
	if err == nil {
		return custID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	created, err := s.stripe.CreateCustomer(ctx, workspaceID)
	if err != nil {
		return "", err
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO billing_customers (workspace_id, stripe_customer_id)
		 VALUES ($1, $2) ON CONFLICT (workspace_id) DO NOTHING`, workspaceID, created); err != nil {
		return "", err
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT stripe_customer_id FROM billing_customers WHERE workspace_id = $1`, workspaceID).Scan(&custID); err != nil {
		return "", err
	}
	return custID, nil
}

// HandleWebhook is the public POST /v1/billing/webhook handler. It reads the RAW
// body (Stripe signs raw bytes) BEFORE any JSON, verifies the signature, and
// dispatches. It returns 200 ONLY when the event's outcome is durably recorded
// (credited / anomalous-claimed / refund-marked / deliberately ignored); a DB or
// credit failure returns 5xx so Stripe redelivers (the claim is durable only on
// commit, so a retry legitimately re-processes).
func (s *Service) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		s.log.Error("billing webhook: body read failed", "err", err)
		http.Error(w, "read error", http.StatusServiceUnavailable) // retry
		return
	}
	// Verify the signature + timestamp, but TOLERATE API-version skew: real Stripe
	// events carry the account's API version, which may differ from the version
	// stripe-go/v81 pins — rejecting on that would 400 every legitimate webhook.
	// The handler reads only version-stable fields (id, type, amount_total,
	// currency, payment_status, metadata, payment_intent).
	event, err := webhook.ConstructEventWithOptions(raw, r.Header.Get("Stripe-Signature"), s.webhookSecret,
		webhook.ConstructEventOptions{IgnoreAPIVersionMismatch: true})
	if err != nil {
		// Bad signature → 400, NO body echo, ZERO db writes. (Do not log the body.)
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}

	switch event.Type {
	case "checkout.session.completed", "checkout.session.async_payment_succeeded":
		s.handleSessionCredit(w, r.Context(), &event)
	case "charge.refunded":
		s.handleRefund(w, r.Context(), &event)
	case "checkout.session.async_payment_failed":
		w.WriteHeader(http.StatusOK) // no money moved → ack, no action
	default:
		w.WriteHeader(http.StatusOK) // unhandled type → ack, no action
	}
}

func (s *Service) handleSessionCredit(w http.ResponseWriter, ctx context.Context, event *stripe.Event) {
	var sess stripe.CheckoutSession
	if err := json.Unmarshal(event.Data.Raw, &sess); err != nil {
		// Signed but unparseable — it will never parse; ack so Stripe stops retrying.
		s.log.Warn("billing webhook: unparseable checkout session", "event", event.ID)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Delayed payment methods fire checkout.session.completed with
	// payment_status != "paid" FIRST, then a SEPARATE
	// async_payment_succeeded (different event id) when money settles. Credit ONLY
	// on paid, and do NOT consume an idempotency id for an unpaid completed — the
	// async event is the one that credits.
	if event.Type == "checkout.session.completed" && sess.PaymentStatus != stripe.CheckoutSessionPaymentStatusPaid {
		w.WriteHeader(http.StatusOK)
		return
	}

	wsID := sess.Metadata["workspace_id"]
	metaLXC, _ := strconv.ParseInt(sess.Metadata["lxc_amount"], 10, 64) // µLXC (SEC-2)
	usdCents := sess.AmountTotal
	recomp := lxcForCents(usdCents)

	anomaly, rerr := s.classify(ctx, string(sess.Currency), usdCents, wsID, recomp, metaLXC)
	if rerr != nil {
		// Could not VERIFY (e.g. workspace lookup failed) — do not guess; retry.
		s.fail(w, "classify", event.ID, rerr)
		return
	}

	pi := ""
	if sess.PaymentIntent != nil {
		pi = sess.PaymentIntent.ID
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.fail(w, "begin", event.ID, err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	status, creditAmt := "completed", recomp
	if anomaly != "" {
		status, creditAmt = "anomalous", 0
	}

	ct, err := tx.Exec(ctx, `
		INSERT INTO lxc_purchases
			(stripe_event_id, stripe_session_id, stripe_payment_intent, workspace_id, usd_cents, lxc_amount, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (stripe_event_id) DO NOTHING`,
		event.ID, sess.ID, pi, wsID, usdCents, creditAmt, status)
	if err != nil {
		// A unique violation here is NOT on the ON CONFLICT target — it is the
		// partial index idx_lxc_purchases_session_credited: this SESSION already
		// has a crediting row (e.g. completed(paid) already credited, then a stray
		// async_succeeded). Durably recorded already → ack, no new credit.
		if isUniqueViolation(err) {
			_ = tx.Rollback(ctx)
			s.log.Warn("billing webhook: duplicate session credit blocked",
				"event", event.ID, "session", sess.ID)
			w.WriteHeader(http.StatusOK)
			return
		}
		s.fail(w, "claim insert", event.ID, err)
		return
	}
	if ct.RowsAffected() == 0 {
		// Same event id re-delivered → already processed. Ack, no new credit.
		_ = tx.Rollback(ctx)
		w.WriteHeader(http.StatusOK)
		return
	}

	if anomaly != "" {
		if err := tx.Commit(ctx); err != nil {
			s.fail(w, "commit anomaly", event.ID, err)
			return
		}
		// CHARGED but NOT credited — surfaced in the admin purchases list; v1
		// resolution is a manual refund in the Stripe dashboard.
		s.log.Warn("billing anomaly: charged, NOT credited",
			"reason", anomaly, "workspace", wsID, "event", event.ID, "session", sess.ID, "usd_cents", usdCents)
		w.WriteHeader(http.StatusOK)
		return
	}

	if _, err := s.credits.CreditLXCTx(ctx, tx, wsID, recomp, "stripe top-up", map[string]interface{}{
		"usd_cents":         usdCents,
		"stripe_event_id":   event.ID,
		"stripe_session_id": sess.ID,
	}); err != nil {
		s.fail(w, "credit", event.ID, err) // rollback → claim not durable → Stripe retries
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.fail(w, "commit credit", event.ID, err)
		return
	}
	// U6 PR2: best-effort owner-linkage capture. Runs AFTER the credit committed
	// above, has no error channel back here, and the 200 is acked regardless — a
	// capture failure can NEVER drop or delay the payment credit. (Worst case, a
	// slow Stripe call → Stripe re-delivers → the stripe_event_id claim above
	// suppresses a second credit.)
	s.captureCardFingerprint(ctx, wsID, pi)
	w.WriteHeader(http.StatusOK)
}

// captureCardFingerprint stores a HASH of the payment's card fingerprint (never
// the raw value) as a U6 PR2 owner-linkage signal. STRUCTURALLY best-effort:
// called only after the LXC credit is durable, returns nothing, and swallows
// every failure (no payment-intent, Stripe API error, no card on the PM, store
// error) with a WARN — it cannot affect the payment.
func (s *Service) captureCardFingerprint(ctx context.Context, wsID, paymentIntentID string) {
	if wsID == "" || paymentIntentID == "" {
		return
	}
	fp, err := s.stripe.CardFingerprint(ctx, paymentIntentID)
	if err != nil || fp == "" {
		s.log.Warn("billing: card-fingerprint capture skipped (best-effort; credit unaffected)",
			"workspace", wsID, "err", errString(err))
		return
	}
	sum := sha256.Sum256([]byte(fp))
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO workspace_card_fingerprints (workspace_id, fingerprint_hash)
		 VALUES ($1, $2) ON CONFLICT DO NOTHING`, wsID, hex.EncodeToString(sum[:])); err != nil {
		s.log.Warn("billing: card-fingerprint store failed (best-effort)", "workspace", wsID, "err", err.Error())
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *Service) handleRefund(w http.ResponseWriter, ctx context.Context, event *stripe.Event) {
	var ch stripe.Charge
	if err := json.Unmarshal(event.Data.Raw, &ch); err != nil {
		s.log.Warn("billing webhook: unparseable charge.refunded", "event", event.ID)
		w.WriteHeader(http.StatusOK)
		return
	}
	pi := ""
	if ch.PaymentIntent != nil {
		pi = ch.PaymentIntent.ID
	}
	if pi == "" {
		w.WriteHeader(http.StatusOK) // nothing to correlate → ack
		return
	}
	// v1: mark refunded only, NO clawback. Idempotent via the refunded_at guard;
	// lxc_amount is kept so the session can never re-credit (partial-index backstop).
	if _, err := s.pool.Exec(ctx, `
		UPDATE lxc_purchases SET status = 'refunded', refunded_at = NOW()
		WHERE stripe_payment_intent = $1 AND refunded_at IS NULL`, pi); err != nil {
		s.fail(w, "refund mark", event.ID, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// classify returns a non-empty anomaly reason when the (signed) event must NOT be
// credited, or a non-nil error when the outcome cannot be VERIFIED (caller retries
// with 5xx rather than guessing). NEVER trusts session metadata as the amount.
func (s *Service) classify(ctx context.Context, currency string, usdCents int64, wsID string, recomp, metaLXC int64) (string, error) {
	if !strings.EqualFold(currency, "usd") {
		return "currency", nil
	}
	if usdCents <= 0 {
		return "nonpositive_amount", nil
	}
	if _, ok := s.allowList[usdCents]; !ok {
		return "amount_not_allowlisted", nil
	}
	if wsID == "" {
		return "unknown_workspace", nil
	}
	ok, err := s.wsExists(ctx, wsID)
	if err != nil {
		return "", err // cannot verify → retry, do not mark anomalous
	}
	if !ok {
		return "unknown_workspace", nil
	}
	// SEC-2: µLXC is an exact integer, so the server recomputation and the
	// client-metadata value must be BIT-EQUAL (the float-epsilon check is gone).
	if recomp != metaLXC {
		return "amount_mismatch", nil
	}
	return "", nil
}

func (s *Service) fail(w http.ResponseWriter, stage, eventID string, err error) {
	// 5xx → Stripe redelivers. err is a DB/infra error, never a secret.
	s.log.Error("billing webhook failed", "stage", stage, "event", eventID, "err", err)
	http.Error(w, "temporary error", http.StatusServiceUnavailable)
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// Purchase is one lxc_purchases row, for the admin refund-visibility list.
type Purchase struct {
	ID              string     `json:"id"`
	StripeEventID   string     `json:"stripe_event_id"`
	StripeSessionID string     `json:"stripe_session_id"`
	WorkspaceID     string     `json:"workspace_id"`
	USDCents        int64      `json:"usd_cents"`
	LXCAmount       int64      `json:"lxc_amount_ulxc"` // µLXC (SEC-2)
	Status          string     `json:"status"`
	CreatedAt       time.Time  `json:"created_at"`
	RefundedAt      *time.Time `json:"refunded_at,omitempty"`
}

// ListPurchases returns recent purchase rows, newest first (admin, read-only).
func (s *Service) ListPurchases(ctx context.Context, limit int) ([]Purchase, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, stripe_event_id, stripe_session_id, workspace_id, usd_cents, lxc_amount, status, created_at, refunded_at
		FROM lxc_purchases ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Purchase, 0, limit)
	for rows.Next() {
		var p Purchase
		if err := rows.Scan(&p.ID, &p.StripeEventID, &p.StripeSessionID, &p.WorkspaceID,
			&p.USDCents, &p.LXCAmount, &p.Status, &p.CreatedAt, &p.RefundedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
