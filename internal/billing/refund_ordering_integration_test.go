package billing

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stripe/stripe-go/v81/webhook"

	"github.com/talyvor/lens/internal/earnverify"
)

// Refund ordering + partial refunds.
//
// ⚠ WHY THESE ARE HERE AND NOT IN A SCRIPT. Both were found by driving the real
// binary over HTTP from a shell rig. A rig proves a thing once; a test proves it on
// every push. The rig's value was discovery, and its findings belong where they
// cannot be lost — beside the money-path tests that already cover the in-order case.
//
// The Stripe-Signature headers come from `signed`, which uses the SDK's OWN signer
// (webhook.ComputeSignature), so these can never drift from what ConstructEvent
// verifies. A hand-rolled signer would be testing my arithmetic, not the handler.

// signedLive is `signed` with livemode=true on the EVENT.
//
// ⚠ IT EXISTS SO ONE ASSERTION IS NOT VACUOUS. The shared `signed` helper omits
// livemode, so events default to livemode=false — and a livemode=false purchase is
// refused by REQUIRE_LIVE=true anyway. Asserting "no earning rights under REQUIRE_LIVE"
// on such an event would pass no matter what this handler did. Real money is
// livemode=true, and that is the case worth pinning.
func signedLive(secret, eventID, eventType string, object map[string]any) ([]byte, string) {
	body, _ := json.Marshal(map[string]any{
		"id":       eventID,
		"type":     eventType,
		"livemode": true,
		"data":     map[string]any{"object": object},
	})
	now := time.Now()
	sig := webhook.ComputeSignature(now, body, secret)
	return body, fmt.Sprintf("t=%d,v1=%s", now.Unix(), hex.EncodeToString(sig))
}

// mayEarn asks the REAL earn predicate — the one the mint gate consults
// (internal/mining/mint_gate.go) — rather than re-deriving its SQL here.
func mayEarn(t *testing.T, pool *pgxpool.Pool, ws string, requireLive bool) bool {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ok, err := earnverify.New(requireLive).MayEarn(ctx, tx, ws)
	if err != nil {
		t.Fatalf("MayEarn: %v", err)
	}
	return ok
}

// TestWebhook_RefundBeforePurchase_NoCreditAndNoEarningRights pins the ordering hazard.
//
// Stripe does not guarantee event ordering, and this needs no exotic race: a refund
// issued moments after payment, or a checkout.session.completed that 5xx'd once and is
// redelivered after the refund, produces exactly this sequence.
//
// Before the fix the refund UPDATE matched no row, returned 200 and recorded NOTHING,
// so the purchase then landed and credited normally — and because a `completed` row is
// the earn-verification evidence, the workspace also kept permanent earning rights that
// LENS_EARN_REQUIRE_LIVE_PURCHASE=true does not revoke.
func TestWebhook_RefundBeforePurchase_NoCreditAndNoEarningRights(t *testing.T) {
	svc, pool, _ := newBillingService(t)
	const ws, sess, pi = "ws_refund_first", "cs_refund_first", "pi_refund_first"
	seedWS(t, pool, ws)

	// 1. The refund arrives FIRST, for a payment intent we have never seen.
	rb, rsig := signed(testWebhookSecret, "evt_rf_chg", "charge.refunded",
		map[string]any{"id": "ch_rf", "payment_intent": pi, "refunded": true,
			"amount": 5000, "amount_refunded": 5000})
	if got := post(svc, rb, rsig); got != http.StatusOK {
		t.Fatalf("early refund must be acked so Stripe stops retrying: got %d", got)
	}

	// 2. The purchase event lands afterwards.
	body, sig := signedLive(testWebhookSecret, "evt_rf_pay", "checkout.session.completed",
		sessionObj(sess, ws, 5000, "usd", "paid", pi, micro(500)))
	if got := post(svc, body, sig); got != http.StatusOK {
		t.Fatalf("purchase: got %d", got)
	}

	// THE MONEY ASSERTION: refunded money must not become spendable credit.
	if b := balance(t, pool, ws); b != 0 {
		t.Errorf("credited %v µLXC for a payment that was already refunded; want 0", b)
	}

	// THE RIGHTS ASSERTION: nor may it confer earning rights. This is the half that
	// survives a balance fix, because the mint gate reads the PURCHASE ROW, not the
	// balance — and it is not closed by requiring live purchases.
	if mayEarn(t, pool, ws, false) {
		t.Error("a refunded purchase conferred earning rights (REQUIRE_LIVE=false)")
	}
	if mayEarn(t, pool, ws, true) {
		t.Error("a refunded purchase conferred earning rights even under REQUIRE_LIVE=true")
	}

	// And the record must SAY so, or the next reader cannot tell this from a purchase
	// that was never made.
	assertStatus(t, pool, sess, "refunded")
	var refundedAt *string
	if err := pool.QueryRow(context.Background(),
		`SELECT refunded_at::text FROM lxc_purchases WHERE stripe_session_id=$1`, sess).Scan(&refundedAt); err != nil {
		t.Fatalf("read refunded_at: %v", err)
	}
	if refundedAt == nil {
		t.Error("refunded_at is NULL on a purchase whose refund we had already received")
	}
}

// TestWebhook_RefundForUnknownPaymentIntent_IsRecordedNotDropped.
//
// A refund may legitimately name a charge this deployment never handled — a charge
// created outside Lens, or one predating a restore. It must still be acked (or Stripe
// retries for days), but acking is not the same as forgetting: the record is what makes
// the ordering fix work, and it is the evidence that the money went back.
func TestWebhook_RefundForUnknownPaymentIntent_IsRecordedNotDropped(t *testing.T) {
	svc, pool, _ := newBillingService(t)
	const pi = "pi_never_seen"

	rb, rsig := signed(testWebhookSecret, "evt_orphan", "charge.refunded",
		map[string]any{"id": "ch_orphan", "payment_intent": pi, "refunded": true,
			"amount": 1000, "amount_refunded": 1000})
	if got := post(svc, rb, rsig); got != http.StatusOK {
		t.Fatalf("orphan refund must be acked: got %d", got)
	}

	var fully bool
	var cents int64
	if err := pool.QueryRow(context.Background(),
		`SELECT fully_refunded, amount_refunded_cents FROM billing_refunds
		 WHERE stripe_payment_intent=$1`, pi).Scan(&fully, &cents); err != nil {
		t.Fatalf("the refund was acked but not recorded: %v", err)
	}
	if !fully || cents != 1000 {
		t.Errorf("recorded fully=%v cents=%d, want true/1000", fully, cents)
	}
}

// TestWebhook_RefundBeforePurchase_PositiveControl is the discriminator: with NO early
// refund, the identical purchase must credit and must confer earning rights.
//
// Without this, the test above passes just as well against a handler that credits
// nothing at all — "no credit" is only meaningful next to a case that does credit.
func TestWebhook_RefundBeforePurchase_PositiveControl(t *testing.T) {
	svc, pool, _ := newBillingService(t)
	const ws, sess, pi = "ws_refund_ctrl", "cs_refund_ctrl", "pi_refund_ctrl"
	seedWS(t, pool, ws)

	body, sig := signed(testWebhookSecret, "evt_ctrl_pay", "checkout.session.completed",
		sessionObj(sess, ws, 5000, "usd", "paid", pi, micro(500)))
	if got := post(svc, body, sig); got != http.StatusOK {
		t.Fatalf("purchase: got %d", got)
	}
	if b := balance(t, pool, ws); b != micro(500) {
		t.Fatalf("control: balance=%v µLXC, want %v — the crediting path is not live, "+
			"so the sibling test proves nothing", b, micro(500))
	}
	if !mayEarn(t, pool, ws, false) {
		t.Error("control: an un-refunded purchase must confer earning rights")
	}
	assertStatus(t, pool, sess, "completed")
}

// TestWebhook_PartialRefund_DoesNotMarkWholePurchaseRefunded.
//
// charge.refunded fires for PARTIAL refunds too. Marking the whole purchase refunded on
// a $1 refund of a $50 charge is only a reporting inaccuracy while there is no clawback
// — and becomes a real one the moment a clawback is built on this status, because the
// obvious implementation claws back the full lxc_amount.
func TestWebhook_PartialRefund_DoesNotMarkWholePurchaseRefunded(t *testing.T) {
	svc, pool, _ := newBillingService(t)
	const ws, sess, pi = "ws_partial", "cs_partial", "pi_partial"
	seedWS(t, pool, ws)

	body, sig := signed(testWebhookSecret, "evt_pr_pay", "checkout.session.completed",
		sessionObj(sess, ws, 5000, "usd", "paid", pi, micro(500)))
	if got := post(svc, body, sig); got != http.StatusOK {
		t.Fatalf("purchase: got %d", got)
	}

	// $1.00 refunded off a $50.00 charge. `refunded` is Stripe's fully-refunded flag.
	rb, rsig := signed(testWebhookSecret, "evt_pr_chg", "charge.refunded",
		map[string]any{"id": "ch_pr", "payment_intent": pi, "refunded": false,
			"amount": 5000, "amount_refunded": 100})
	if got := post(svc, rb, rsig); got != http.StatusOK {
		t.Fatalf("partial refund: got %d", got)
	}

	assertStatus(t, pool, sess, "completed")
	var cents int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(refunded_cents,0) FROM lxc_purchases WHERE stripe_session_id=$1`, sess).Scan(&cents); err != nil {
		t.Fatalf("read refunded_cents: %v", err)
	}
	if cents != 100 {
		t.Errorf("refunded_cents=%d, want 100 — the partial refund must be RECORDED, not discarded", cents)
	}
	// Partial refunds leave earning rights intact: money was paid and kept.
	if !mayEarn(t, pool, ws, false) {
		t.Error("a partial refund revoked earning rights for a purchase that was mostly paid")
	}
}

// TestWebhook_PartialThenFullRefund_EndsRefunded — a partial followed by the rest is a
// common Stripe sequence; the terminal state must be `refunded`.
func TestWebhook_PartialThenFullRefund_EndsRefunded(t *testing.T) {
	svc, pool, _ := newBillingService(t)
	const ws, sess, pi = "ws_p2f", "cs_p2f", "pi_p2f"
	seedWS(t, pool, ws)

	body, sig := signed(testWebhookSecret, "evt_p2f_pay", "checkout.session.completed",
		sessionObj(sess, ws, 5000, "usd", "paid", pi, micro(500)))
	post(svc, body, sig)

	rb, rsig := signed(testWebhookSecret, "evt_p2f_1", "charge.refunded",
		map[string]any{"id": "ch_p2f", "payment_intent": pi, "refunded": false,
			"amount": 5000, "amount_refunded": 100})
	post(svc, rb, rsig)
	assertStatus(t, pool, sess, "completed")

	rb2, rsig2 := signed(testWebhookSecret, "evt_p2f_2", "charge.refunded",
		map[string]any{"id": "ch_p2f", "payment_intent": pi, "refunded": true,
			"amount": 5000, "amount_refunded": 5000})
	if got := post(svc, rb2, rsig2); got != http.StatusOK {
		t.Fatalf("full refund: got %d", got)
	}
	assertStatus(t, pool, sess, "refunded")
	if mayEarn(t, pool, ws, false) {
		t.Error("a fully-refunded purchase still conferred earning rights")
	}
}
