-- 0027_lxc_credits.sql — two-token split (Master Plan Upgrade 1).
--
-- Splits the single conflated LENS token into two:
--   LENS — volatile, tradeable, mined. (Existing token, role-
--          clarified. lens_token_balances / lens_token_ledger
--          from 0019 are UNCHANGED by this migration.)
--   LXC  — USD-pegged compute credit. 1 LXC = $0.10 of compute.
--          Non-tradeable, one-way: minted by converting LENS (or
--          purchasing), spent on AI calls, NEVER refundable to
--          fiat or back to LENS.
--
-- MIGRATION OF EXISTING BALANCES (one-time, intentional no-op):
--   - Existing LENS balances stay as LENS at 1:1. They are NOT
--     auto-split into LXC. The role of the existing token simply
--     changes from "conflated stablecoin + asset" to "volatile
--     mined asset".
--   - Every workspace starts with 0 LXC.
--   - Workspaces convert LENS -> LXC themselves, on demand, at
--     the admin-approved conversion rate, when they want spendable
--     compute credit.
-- This file therefore only CREATES the new LXC tables — it
-- deliberately touches nothing under lens_token_*.

CREATE TABLE IF NOT EXISTS lxc_balances (
    workspace_id    TEXT PRIMARY KEY,
    balance         DOUBLE PRECISION NOT NULL DEFAULT 0,
    lifetime_minted DOUBLE PRECISION NOT NULL DEFAULT 0,
    lifetime_spent  DOUBLE PRECISION NOT NULL DEFAULT 0,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS lxc_ledger (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  TEXT NOT NULL,
    amount        DOUBLE PRECISION NOT NULL,  -- + mint, - spend
    balance_after DOUBLE PRECISION NOT NULL,
    type          TEXT NOT NULL,              -- convert_from_lens | spend | purchase
    description   TEXT NOT NULL DEFAULT '',
    metadata      JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_lxc_ledger_workspace
    ON lxc_ledger (workspace_id, created_at DESC);

-- conversion_rate_history is the audit trail for every approved
-- LENS->LXC rate. Each row records all the intermediate values
-- the engine used so a rate change is fully reconstructable. The
-- admin can only APPROVE the algorithm's output — there is no
-- column for an arbitrary operator-set rate.
CREATE TABLE IF NOT EXISTS conversion_rate_history (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    rate          DOUBLE PRECISION NOT NULL,  -- LENS per 1 LXC (R_admin)
    fair_rate     DOUBLE PRECISION NOT NULL,  -- R_fair before spread
    backing_value DOUBLE PRECISION NOT NULL,  -- USD value per LENS
    circulating   DOUBLE PRECISION NOT NULL,  -- circulating LENS supply
    spread        DOUBLE PRECISION NOT NULL,  -- policy spread applied
    previous_rate DOUBLE PRECISION NOT NULL,
    approved_by   TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_rate_history_created
    ON conversion_rate_history (created_at DESC);
