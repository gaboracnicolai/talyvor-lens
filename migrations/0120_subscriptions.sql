-- 0120_subscriptions.sql — Model 2 step 1: Stripe SUBSCRIPTIONS, on test keys.
--
-- W4.6.1 step 1. Everything Stripe billing does today is a ONE-OFF payment:
-- `lxc_purchases` records a completed Checkout Session and credits LXC. There is
-- no recurring anything — measured at `6b1f99d`, `customer.subscription` appears
-- in ZERO non-test Go files and the webhook switch handles only
-- checkout.session.* and charge.refunded.
--
-- TWO TABLES, BECAUSE THEY ANSWER TWO DIFFERENT QUESTIONS:
--
--   subscriptions        — the CURRENT state of one Stripe subscription. One row
--                          per stripe_subscription_id, mutated in place.
--   subscription_events  — APPEND-ONLY. Every signed webhook that was about a
--                          subscription, whether or not it changed the state.
--
-- ⚠ THE EVENT TABLE IS NOT AN AUDIT LOG NICETY, IT IS THE IDEMPOTENCY KEY AND THE
-- THING TESTS ASSERT. W4.6.1: "ASSERT THE LEDGER ROW, NEVER A STATUS CODE."
-- Stripe retries webhooks; a handler that returns 200 twice has told you nothing
-- about whether it acted twice. `stripe_event_id` is UNIQUE, so the second
-- delivery of the same event cannot create a second row, and a test can prove
-- the difference between "acked" and "applied" by reading `applied`.
--
-- ⚠ AND `applied` IS THE COLUMN THAT MAKES A STALE EVENT VISIBLE. Stripe does not
-- guarantee delivery ORDER. A `customer.subscription.updated` carrying
-- status=past_due can arrive AFTER the `.updated` that already moved the row back
-- to active, and a naive handler — the one this item warns "is where every naive
-- implementation breaks" — would happily write the older status over the newer
-- one and dun a customer who has paid. So `subscriptions.last_event_at` holds the
-- Stripe `event.created` of the last event APPLIED, an out-of-order event is
-- recorded with applied=false, and the state is left alone.

CREATE TABLE IF NOT EXISTS subscriptions (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id           TEXT NOT NULL,
    stripe_subscription_id TEXT NOT NULL UNIQUE,
    stripe_customer_id     TEXT NOT NULL,
    -- The Stripe Price the subscription is on. F (the fee) is Nicolai's number and
    -- is NOT hardcoded anywhere: this records which price Stripe actually billed.
    price_id               TEXT NOT NULL,
    -- Stripe's own vocabulary, not a translation of it. A second spelling of a
    -- state machine somebody else owns is a second thing to keep in sync.
    status                 TEXT NOT NULL
        CHECK (status IN ('incomplete', 'incomplete_expired', 'trialing', 'active',
                          'past_due', 'canceled', 'unpaid', 'paused')),
    current_period_end     TIMESTAMPTZ,
    cancel_at_period_end   BOOLEAN NOT NULL DEFAULT FALSE,
    -- From the SIGNED event, never inferred from which API key is configured —
    -- the same rule lxc_purchases.livemode follows, and the reason a test-mode
    -- subscription can be recorded without conferring live-mode rights.
    livemode               BOOLEAN NOT NULL,
    -- Stripe `event.created` of the last event that MOVED this row. See the
    -- out-of-order note above.
    last_event_at          TIMESTAMPTZ NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ⚠ ONE LIVE SUBSCRIPTION PER WORKSPACE, ENFORCED BY THE DATABASE RATHER THAN BY
-- REMEMBERING. A workspace that checks out twice before the first webhook lands
-- would otherwise end up billed twice with two `active` rows and no way to say
-- which one the allowance (step 2) should read. Terminal states are excluded, so
-- a workspace can resubscribe after cancelling — the old row stays as history.
CREATE UNIQUE INDEX IF NOT EXISTS idx_subscriptions_one_live_per_workspace
    ON subscriptions (workspace_id)
    WHERE status IN ('trialing', 'active', 'past_due', 'unpaid');

CREATE INDEX IF NOT EXISTS idx_subscriptions_workspace
    ON subscriptions (workspace_id, created_at DESC);

CREATE TABLE IF NOT EXISTS subscription_events (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- The idempotency key. UNIQUE, so a Stripe retry cannot act twice.
    stripe_event_id        TEXT NOT NULL UNIQUE,
    stripe_subscription_id TEXT NOT NULL,
    workspace_id           TEXT NOT NULL,
    event_type             TEXT NOT NULL,
    -- The status this event asked for, and whether it was taken. `applied=false`
    -- with a status_requested that differs from the row's status is exactly the
    -- out-of-order case, and it is recorded rather than dropped so an operator can
    -- see that Stripe sent it and that we deliberately refused it.
    status_requested       TEXT NOT NULL,
    applied                BOOLEAN NOT NULL,
    livemode               BOOLEAN NOT NULL,
    event_created_at       TIMESTAMPTZ NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_subscription_events_sub
    ON subscription_events (stripe_subscription_id, event_created_at DESC);

CREATE INDEX IF NOT EXISTS idx_subscription_events_workspace
    ON subscription_events (workspace_id, created_at DESC);
