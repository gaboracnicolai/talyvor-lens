-- 0054_lxc_purchases.sql — U18b billing core: fiat (Stripe) → LXC credit.
--
-- lxc_purchases is the idempotency CLAIM + purchase record for paid LXC top-ups,
-- the #0043 claim-then-credit shape: the webhook INSERTs a claim row ON CONFLICT
-- (stripe_event_id) DO NOTHING and credits LXC ONLY when the insert claimed the
-- row (RowsAffected == 1), in the SAME transaction as the CreditLXCTx ledger
-- write. stripe_event_id ALONE is the primary idempotency key — Stripe event ids
-- are immutable and unique per delivery, so a duplicate/concurrent re-delivery of
-- the SAME event can only SUPPRESS a second credit (the UNIQUE index serializes
-- concurrent inserts), never inflate it.
--
-- The second backstop — idx_lxc_purchases_session_credited — closes the
-- delayed-payment double-credit: for async payment methods Stripe fires
-- checkout.session.completed (payment_status="unpaid") FIRST and then a SEPARATE
-- checkout.session.async_payment_succeeded event (a DIFFERENT event id) when the
-- money actually settles. The event-id key does NOT protect across those two, so
-- a partial UNIQUE on stripe_session_id WHERE lxc_amount > 0 enforces "at most one
-- CREDITING row per checkout session, ever". Anomalous rows (lxc_amount = 0) are
-- exempt (a session can legitimately log an anomaly then later credit); refunded
-- rows KEEP their lxc_amount, so a refunded session can never re-credit. A second
-- crediting INSERT for the same session raises a unique violation on THIS index
-- (not the ON CONFLICT target) → the tx rolls back → no credit.
--
-- Money is integer cents (usd_cents); lxc_amount is ALWAYS the server-side
-- recomputation usd/peg, never trusted from Stripe session metadata. UNPARTITIONED
-- like pool_royalty_mints / pattern_mine_credits (a bare/partial UNIQUE is illegal
-- on the hash-partitioned hot tables). Additive only — no change to existing
-- tables, no advisory locks (within the migration-audit invariants).
CREATE TABLE IF NOT EXISTS lxc_purchases (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    stripe_event_id       TEXT NOT NULL UNIQUE,                -- idempotency claim key (one credit per completion event)
    stripe_session_id     TEXT NOT NULL DEFAULT '',            -- checkout.session.id (audit / session-credit backstop)
    stripe_payment_intent TEXT NOT NULL DEFAULT '',            -- links a later charge.refunded back to this row
    workspace_id          TEXT NOT NULL,
    usd_cents             BIGINT NOT NULL,                     -- integer cents charged (no float money)
    lxc_amount            DOUBLE PRECISION NOT NULL DEFAULT 0, -- server-recomputed LXC credited (0 when anomalous)
    status                TEXT NOT NULL DEFAULT 'completed',   -- completed | refunded | anomalous
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    refunded_at           TIMESTAMPTZ NULL
);

-- At most ONE crediting row per session, ever (the delayed-payment double-credit
-- backstop). Partial: anomalous rows (lxc_amount = 0) are exempt.
CREATE UNIQUE INDEX IF NOT EXISTS idx_lxc_purchases_session_credited
    ON lxc_purchases (stripe_session_id) WHERE lxc_amount > 0;

CREATE INDEX IF NOT EXISTS idx_lxc_purchases_workspace
    ON lxc_purchases (workspace_id, created_at DESC);

-- charge.refunded correlates by payment-intent (the refund event carries no
-- session id), so the refund UPDATE keys on this column.
CREATE INDEX IF NOT EXISTS idx_lxc_purchases_payment_intent
    ON lxc_purchases (stripe_payment_intent);

-- billing_customers — workspace ↔ Stripe customer mapping. Kept OUT of the
-- workspace hot map so Stripe PII (customer id) never rides the request path.
CREATE TABLE IF NOT EXISTS billing_customers (
    workspace_id       TEXT PRIMARY KEY,
    stripe_customer_id TEXT NOT NULL UNIQUE,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
