-- B20.5 — sellers are paid in money, through Stripe Connect.
--
-- A seller connects a Stripe Express account (Stripe's onboarding collects their identity, bank and tax
-- details) and Lens records what Stripe says of it. Once a month, a seller's earnings past their 14-day
-- holdback are paid out when they reach US$25: one market_payouts row, then one Stripe transfer to their
-- account for the payout less Stripe's fees at cost. A seller may instead take what is available as
-- Talyvor credits, 1:1, whenever they like — a market_payouts row and one lxc_ledger row, in one
-- transaction.
--
-- A seller's available balance is what they earned past the holdback, less what refunds and chargebacks
-- reversed (market_refunds), less every payout. A buyer's refund or chargeback reverses the earnings of
-- the uses it paid for: inside the holdback they never become available; after it, the balance falls, and
-- when it falls below zero the seller owes it and it is recovered from their future earnings.

-- Each seller's connected Stripe account, and what Stripe last said about it.
CREATE TABLE IF NOT EXISTS market_sellers (
    workspace_id      TEXT PRIMARY KEY,
    stripe_account_id TEXT NOT NULL UNIQUE,
    country           TEXT NOT NULL DEFAULT '',
    details_submitted BOOLEAN NOT NULL DEFAULT false,
    payouts_enabled   BOOLEAN NOT NULL DEFAULT false,
    currently_due     TEXT[] NOT NULL DEFAULT '{}',  -- what Stripe still needs from them
    disabled_reason   TEXT NOT NULL DEFAULT '',
    checked_at        TIMESTAMPTZ,                   -- when Stripe was last asked, or last told us
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Every payout, append-only: money through Stripe, or credits. gross leaves the seller's balance; the
-- fees are Stripe's, at cost; net is what the seller receives.
CREATE TABLE IF NOT EXISTS market_payouts (
    id                     TEXT PRIMARY KEY,          -- mpo_<uuid>; the transfer's transfer_group and idempotency key
    workspace_id           TEXT NOT NULL,             -- the seller
    method                 TEXT NOT NULL CHECK (method IN ('stripe', 'credits')),
    month                  TEXT NOT NULL,             -- YYYY-MM (UTC): one Stripe payout a month
    gross_usd_micros       BIGINT NOT NULL CHECK (gross_usd_micros > 0),
    account_fee_usd_micros BIGINT NOT NULL DEFAULT 0 CHECK (account_fee_usd_micros >= 0), -- Stripe: $2 per account paid in a month
    payout_fee_usd_micros  BIGINT NOT NULL DEFAULT 0 CHECK (payout_fee_usd_micros >= 0),  -- Stripe: 0.25% + $0.25 per payout
    net_usd_micros         BIGINT NOT NULL CHECK (net_usd_micros > 0),
    credits_ulxc           BIGINT NOT NULL DEFAULT 0 CHECK (credits_ulxc >= 0),          -- method credits: what the ledger was credited
    stripe_account_id      TEXT,
    stripe_transfer_id     TEXT,                      -- set once Stripe accepted the transfer
    paid_at                TIMESTAMPTZ,               -- credits: at once; stripe: when the transfer was accepted
    last_error             TEXT NOT NULL DEFAULT '',  -- why Stripe has not accepted it yet; the next pass retries
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (gross_usd_micros = account_fee_usd_micros + payout_fee_usd_micros + net_usd_micros),
    CHECK ((method = 'stripe') = (stripe_account_id IS NOT NULL)),
    CHECK (method = 'stripe' OR (paid_at IS NOT NULL AND credits_ulxc > 0))
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_market_payouts_monthly ON market_payouts (workspace_id, month) WHERE method = 'stripe';
CREATE INDEX IF NOT EXISTS idx_market_payouts_seller ON market_payouts (workspace_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_market_payouts_unpaid ON market_payouts (created_at) WHERE paid_at IS NULL;

CREATE OR REPLACE FUNCTION market_payouts_transfer_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.paid_at IS NULL
       AND (to_jsonb(NEW) - 'stripe_transfer_id' - 'paid_at' - 'last_error')
         = (to_jsonb(OLD) - 'stripe_transfer_id' - 'paid_at' - 'last_error') THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'market_payouts is append-only: only an unpaid payout''s Stripe transfer may be recorded on it';
END $$;
CREATE OR REPLACE TRIGGER market_payouts_transfer_only BEFORE UPDATE ON market_payouts
    FOR EACH ROW EXECUTE FUNCTION market_payouts_transfer_only();
CREATE OR REPLACE TRIGGER audit_no_delete BEFORE DELETE ON market_payouts
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON market_payouts
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();

-- A refund's cause. B20.4's takedown credits the buyer on their next bill; a buyer's refund or chargeback
-- was money Stripe already returned, so it reverses the seller's earning and credits nobody. stripe_ref is
-- the Stripe object that caused it (the refunded charge, or the dispute).
ALTER TABLE market_refunds
    ADD COLUMN IF NOT EXISTS cause TEXT NOT NULL DEFAULT 'takedown' CHECK (cause IN ('takedown', 'buyer_refund', 'chargeback')),
    ADD COLUMN IF NOT EXISTS stripe_ref TEXT NOT NULL DEFAULT '';
