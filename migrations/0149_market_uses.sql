-- B20.2 — use a listing, pay per use, and the seller earns.
--
-- A use runs a listing's version for a buyer workspace (or one of its agents). A paid use is METERED onto
-- the buyer's monthly marketplace bill — a Stripe usage-based subscription, one per buyer — never taken
-- from prepaid credits. When the invoice carrying it is paid, the use clears and the seller earns their
-- share in USD: 100% of the first US$1M they earn, then 85%. An earning waits a 14-day holdback before
-- it can be paid out (B20.5).
--
-- charge says what the buyer owes for the use:
--   billed  the listing's price, metered onto the buyer's bill
--   free    the listing is free
--   own     the seller used their own listing: no charge, no earning
--   linked  the buyer and seller share a card or an owner (the single-party detector): a wash trade,
--           so no charge and no earning
CREATE TABLE IF NOT EXISTS market_uses (
    id                  TEXT PRIMARY KEY,           -- use_<uuid>; the meter event's identifier, so it is billed once
    listing_id          TEXT NOT NULL,             -- no foreign key: a seller who leaves takes their listings, not their buyers' bills
    version             INTEGER NOT NULL,
    seller_workspace_id TEXT NOT NULL,
    buyer_workspace_id  TEXT NOT NULL,
    agent_id            TEXT NOT NULL DEFAULT '',  -- the buyer's agent that used it, when an agent's key did
    price_ulxc          BIGINT NOT NULL CHECK (price_ulxc >= 0),
    charge              TEXT NOT NULL CHECK (charge IN ('billed', 'free', 'own', 'linked')),
    used_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    ran_at              TIMESTAMPTZ,               -- the run answered; a use whose run failed is removed, never billed
    metered_at          TIMESTAMPTZ,               -- Stripe accepted the meter event
    cleared_invoice_id  TEXT,                      -- the buyer's paid invoice that carried it
    cleared_at          TIMESTAMPTZ,
    CHECK ((charge = 'billed') = (price_ulxc > 0))
);
CREATE INDEX IF NOT EXISTS idx_market_uses_buyer ON market_uses (buyer_workspace_id, used_at DESC);
CREATE INDEX IF NOT EXISTS idx_market_uses_unmetered ON market_uses (used_at) WHERE charge = 'billed' AND metered_at IS NULL AND ran_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_market_uses_agent ON market_uses (agent_id, used_at) WHERE agent_id <> '';

-- A seller's earnings, append-only like every other money record: one per cleared use.
CREATE TABLE IF NOT EXISTS market_earnings (
    use_id              TEXT PRIMARY KEY,          -- market_uses.id; no foreign key, so a buyer who leaves does not hold a seller's earnings
    seller_workspace_id TEXT NOT NULL,
    gross_usd_micros    BIGINT NOT NULL CHECK (gross_usd_micros >= 0),  -- what the buyer paid for the use
    share_usd_micros    BIGINT NOT NULL CHECK (share_usd_micros >= 0),  -- the seller's share of it
    invoice_id          TEXT NOT NULL,
    cleared_at          TIMESTAMPTZ NOT NULL,
    payable_at          TIMESTAMPTZ NOT NULL  -- cleared_at + the 14-day holdback
);
CREATE INDEX IF NOT EXISTS idx_market_earnings_seller ON market_earnings (seller_workspace_id, cleared_at DESC);

CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON market_earnings
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON market_earnings
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();

-- Each buyer's marketplace bill: the Stripe subscription its uses are metered onto.
CREATE TABLE IF NOT EXISTS market_bills (
    workspace_id           TEXT PRIMARY KEY,
    stripe_customer_id     TEXT NOT NULL,
    stripe_subscription_id TEXT NOT NULL UNIQUE,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
