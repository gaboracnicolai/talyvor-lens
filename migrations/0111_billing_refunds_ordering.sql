-- 0111_billing_refunds_ordering.sql — make refunds independent of event ORDER,
-- and stop a partial refund from voiding a whole purchase.
--
-- ── THE BUG ──────────────────────────────────────────────────────────────────
--
-- handleRefund was a single UPDATE correlated on stripe_payment_intent. When the
-- refund arrived BEFORE its purchase — which Stripe permits, and which needs no
-- exotic race (a refund issued moments after payment, or a
-- checkout.session.completed that 5xx'd once and is redelivered after it) — the
-- UPDATE matched no row, returned 200, and recorded NOTHING. The purchase event
-- then landed and credited normally.
--
-- The cost was two things, not one: spendable LXC for money that went back to the
-- customer, AND — because a `completed` row in lxc_purchases IS the
-- earn-verification evidence (internal/earnverify) — permanent earning rights,
-- which LENS_EARN_REQUIRE_LIVE_PURCHASE=true does NOT revoke.
--
-- ── WHY A TOMBSTONE AND NOT A REPLAY ─────────────────────────────────────────
--
-- The alternatives were considered and rejected in the handler's own comment; the
-- short form: a deferred replay leaves the credit live and spendable for the
-- length of its retry window, and 5xx-ing the unmatched refund outsources our
-- ordering problem to Stripe's retry schedule and then loses it silently when the
-- retries are exhausted. Recording the refund makes BOTH orders converge on the
-- same state, which is the only property worth having on a money path.
--
-- billing_refunds is keyed by payment intent because that is the only identifier
-- the two events share. A row here is NOT evidence of a purchase — it is evidence
-- that money went back, whether or not we ever saw the charge. Rows for payment
-- intents that never arrive are expected and harmless; they are the record of a
-- refund for a charge this deployment did not handle.
CREATE TABLE IF NOT EXISTS billing_refunds (
    stripe_payment_intent TEXT PRIMARY KEY,
    -- Stripe's own fully-refunded flag from the charge. A partial refund is NOT a
    -- voided purchase, so only a full refund suppresses the credit.
    fully_refunded        BOOLEAN     NOT NULL,
    amount_refunded_cents BIGINT      NOT NULL,
    -- The FIRST event that told us. Later charge.refunded events for the same
    -- intent (partial → full) update the amounts but never rewrite this.
    first_event_id        TEXT        NOT NULL,
    received_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ── PARTIAL REFUNDS ──────────────────────────────────────────────────────────
--
-- charge.refunded fires for partial refunds too, and the old handler ignored the
-- amount: $1 refunded off a $50 charge set status='refunded' on the whole row.
-- That was a reporting inaccuracy while there is no clawback, and would have
-- become a real one the moment a clawback was built on this status, because the
-- obvious implementation claws back the full lxc_amount.
--
-- Cumulative, not per-event: Stripe reports the charge's TOTAL amount_refunded on
-- every event, so this column is assigned, never added to. DEFAULT 0 is the honest
-- value for every existing row — no refund has been recorded against them.
ALTER TABLE lxc_purchases
    ADD COLUMN IF NOT EXISTS refunded_cents BIGINT NOT NULL DEFAULT 0;

COMMENT ON COLUMN lxc_purchases.refunded_cents IS
  'Cumulative USD cents refunded against this purchase (Stripe charge.amount_refunded). '
  'status becomes ''refunded'' only on a FULL refund; a partial leaves status intact.';

COMMENT ON TABLE billing_refunds IS
  'Refunds keyed by payment intent, recorded even when no purchase row exists yet — '
  'so a refund that arrives before its checkout.session.completed still suppresses the credit.';
