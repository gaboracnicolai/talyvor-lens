-- B18.14 — a subscriber changes plan (Plus → Pro, or back) mid-period. Stripe prorates the fee; this is
-- the matching move of the period's allowance, one row per customer.subscription.updated that carried a
-- new price for a period already granted: the fees before and after, the share of the period left, and
-- the allowance before and after — so the ledger shows exactly what the change granted or took back.

CREATE TABLE IF NOT EXISTS subscription_plan_changes (
    stripe_event_id        TEXT PRIMARY KEY,                           -- the webhook event that applied it
    workspace_id           TEXT NOT NULL,
    stripe_subscription_id TEXT NOT NULL,
    period_start           TIMESTAMPTZ NOT NULL,
    price_id               TEXT NOT NULL DEFAULT '',                   -- the Stripe Price moved to
    from_fee_usd_cents     BIGINT NOT NULL CHECK (from_fee_usd_cents > 0),
    to_fee_usd_cents       BIGINT NOT NULL CHECK (to_fee_usd_cents > 0),
    remaining_fraction     DOUBLE PRECISION NOT NULL CHECK (remaining_fraction >= 0 AND remaining_fraction <= 1),
    granted_before_ulxc    BIGINT NOT NULL,
    granted_after_ulxc     BIGINT NOT NULL,
    changed_at             TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS subscription_plan_changes_workspace ON subscription_plan_changes (workspace_id, changed_at DESC);
