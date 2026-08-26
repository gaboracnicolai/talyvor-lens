-- 0121_subscription_allowance.sql — Model 2 step 2: THE ALLOWANCE LEDGER.
--
-- W4.6.1 step 2. Step 1 (migration 0120) recorded that a workspace PAYS. This
-- records what paying ENTITLES them to, and — the part that makes the product
-- priceable — the ceiling it can never exceed.
--
-- ── THE ECONOMICS THIS TABLE ENFORCES ──────────────────────────────────────────
--
-- The item states it: phi = F/D < 1, where F is the monthly fee and D is the
-- allowance. The allowance is worth MORE than the fee, which is the offer. What
-- makes that safe rather than reckless is the second half:
--
--     ⚠ HARD CAP, NEVER OVERAGE. WORST CASE PER SUBSCRIBER IS EXACTLY D.
--
-- A subscriber cannot consume D + 1. When the allowance is gone it is gone, and
-- they top up with prepaid LXC at the metered rate. That is the difference between
-- a business that can PRICE its worst case and one that HOPES for its average —
-- and it is why `granted_ulxc` is written once at grant time and `consumed_ulxc`
-- is CHECK-constrained never to pass it. The cap is a database invariant, not a
-- branch someone has to remember.
--
-- ⚠ F AND D ARE NOT IN THIS FILE, AND THE DEFAULT IS OFF. The item says they are
-- Nicolai's. `granted_ulxc` records what was actually granted for a period, so the
-- number lives in the operator's configuration; with no allowance configured
-- (LENS_SUBSCRIPTION_ALLOWANCE_ULXC=0, the default) no row is ever written and the
-- feature is inert. An empty table is the honest state of an undecided price.
--
-- ── WHY A ROW PER PERIOD, NOT A RUNNING BALANCE ────────────────────────────────
--
-- An allowance is "D per billing period", so the period is part of the identity of
-- the grant. A single running balance would need a reset, and a reset is a write
-- that can be missed, run twice, or run late — and each of those is a customer
-- getting a different amount than they paid for. A row per (subscription, period)
-- cannot be reset wrongly because it is never reset: the next period is a new row,
-- and a period with no row simply has no allowance.
--
-- ⚠ AND IT MAKES THE HISTORY READABLE. "How much did this subscriber actually use
-- of what they were given, month over month" is a SELECT, not a reconstruction from
-- a ledger of deltas.

CREATE TABLE IF NOT EXISTS subscription_allowance (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id           TEXT NOT NULL,
    stripe_subscription_id TEXT NOT NULL,

    -- The billing period this grant belongs to. `period_start` comes from Stripe's
    -- subscription period, so a renewal that arrives late still grants against the
    -- period it was for rather than the moment we processed it.
    period_start           TIMESTAMPTZ NOT NULL,
    period_end             TIMESTAMPTZ NOT NULL,

    -- D, in µLXC, as granted for THIS period. Written once. Immutable by rule: the
    -- consume path only ever touches `consumed_ulxc`.
    granted_ulxc           BIGINT NOT NULL CHECK (granted_ulxc > 0),

    -- ⚠ THE HARD CAP, AS A DATABASE INVARIANT. Not "the code checks before writing"
    -- — a check-then-write has a window, and two concurrent settles both pass it.
    -- This constraint holds under any interleaving, and the consume path is written
    -- so that exceeding it is a CLAMP rather than an error: the subscriber gets the
    -- remainder and the excess falls through to prepaid LXC.
    consumed_ulxc          BIGINT NOT NULL DEFAULT 0
        CHECK (consumed_ulxc >= 0 AND consumed_ulxc <= granted_ulxc),

    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ⚠ ONE GRANT PER SUBSCRIPTION PER PERIOD, ENFORCED. A renewal webhook that Stripe
-- redelivers must not grant D twice — that is the same subscriber getting 2D for
-- one fee, and it is exactly the shape the worst-case bound exists to rule out.
CREATE UNIQUE INDEX IF NOT EXISTS idx_allowance_one_grant_per_period
    ON subscription_allowance (stripe_subscription_id, period_start);

-- The read the consume path takes on every settle: "this workspace's current grant".
CREATE INDEX IF NOT EXISTS idx_allowance_workspace_period
    ON subscription_allowance (workspace_id, period_start DESC);
