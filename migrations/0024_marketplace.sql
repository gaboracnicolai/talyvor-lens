-- 0024_marketplace.sql — token marketplace + staking (Batch 3 Phase 1).
--
-- Three tables — each one is the source of truth for its part of
-- the economy:
--   marketplace_listings  — LENS for sale (status driven).
--   marketplace_trades    — executed buys (append-only history).
--   stake_positions       — locked LENS earning yield.
--
-- The LENS itself moves through lens_token_ledger (0019) — these
-- tables only track the marketplace state machine.

CREATE TABLE IF NOT EXISTS marketplace_listings (
    id           TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    seller_id    TEXT NOT NULL,
    amount       DOUBLE PRECISION NOT NULL CHECK (amount > 0),
    price_usd    DOUBLE PRECISION NOT NULL CHECK (price_usd > 0),
    min_buy_usd  DOUBLE PRECISION NOT NULL DEFAULT 0,
    status       TEXT NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'filled', 'cancelled')),
    filled_at    TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Partial price-ascending index for the cheapest-first listing
-- feed. Filled / cancelled rows stay queryable but don't cost
-- index pages.
CREATE INDEX IF NOT EXISTS idx_listings_active
    ON marketplace_listings (price_usd ASC)
    WHERE status = 'active';

CREATE INDEX IF NOT EXISTS idx_listings_seller
    ON marketplace_listings (seller_id, created_at DESC);

CREATE TABLE IF NOT EXISTS marketplace_trades (
    id          TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    listing_id  TEXT NOT NULL REFERENCES marketplace_listings(id),
    buyer_id    TEXT NOT NULL,
    seller_id   TEXT NOT NULL,
    amount      DOUBLE PRECISION NOT NULL,
    price_usd   DOUBLE PRECISION NOT NULL,
    talyvor_fee DOUBLE PRECISION NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_trades_buyer  ON marketplace_trades (buyer_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_trades_seller ON marketplace_trades (seller_id, created_at DESC);

CREATE TABLE IF NOT EXISTS stake_positions (
    id           TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    workspace_id TEXT NOT NULL,
    amount       DOUBLE PRECISION NOT NULL CHECK (amount > 0),
    lock_days    INTEGER NOT NULL CHECK (lock_days IN (30, 90, 180)),
    apy          DOUBLE PRECISION NOT NULL,
    started_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    unlocks_at   TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_stakes_workspace
    ON stake_positions (workspace_id);
