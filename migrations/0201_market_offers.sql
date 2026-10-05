-- B32.18 — offers: pay per use, buy, rent or subscribe, each under a personal, commercial or enterprise licence.
--
-- A listing is sold through its offers (internal/market/offers.go, where the three licences are defined). Each offer
-- is one way to buy it: per_use, buy, rent (for 1 to 365 days) or subscribe (for 30 or 365 days), under one licence,
-- at a price in µUSD. A rent or a subscription may cap its uses (included_uses, 0 = unlimited); an enterprise offer
-- names its seats; a per_use offer may give trial uses (B32.21). A listing has at most one active offer per kind and
-- licence. Replacing a listing's offers ends the old ones (active false) and adds the new: an offer's terms never
-- change, so a licence bought under it (B32.19) keeps the terms it was sold on, and a past use keeps its price.
CREATE TABLE IF NOT EXISTS market_offers (
    id               TEXT PRIMARY KEY,  -- ofr_<uuid>
    listing_id       TEXT NOT NULL REFERENCES market_listings (id) ON DELETE CASCADE,
    kind             TEXT NOT NULL CHECK (kind IN ('per_use', 'buy', 'rent', 'subscribe')),
    licence          TEXT NOT NULL CHECK (licence IN ('personal', 'commercial', 'enterprise')),
    price_usd_micros BIGINT NOT NULL CHECK (price_usd_micros >= 0),
    period_days      INTEGER,
    included_uses    INTEGER,
    seats            INTEGER,
    trial_uses       INTEGER NOT NULL DEFAULT 0,
    active           BOOLEAN NOT NULL DEFAULT true,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (CASE kind WHEN 'rent' THEN coalesce(period_days BETWEEN 1 AND 365, false)
                     WHEN 'subscribe' THEN coalesce(period_days IN (30, 365), false)
                     ELSE period_days IS NULL END),
    CHECK (CASE WHEN kind IN ('rent', 'subscribe') THEN coalesce(included_uses >= 0, false) ELSE included_uses IS NULL END),
    CHECK (CASE WHEN licence = 'enterprise' THEN coalesce(seats >= 1, false) ELSE seats IS NULL END),
    CHECK (trial_uses >= 0 AND (kind = 'per_use' OR trial_uses = 0))
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_market_offers_active ON market_offers (listing_id, kind, licence) WHERE active;

-- An offer is only ever ended, once: its terms and its listing never change.
CREATE OR REPLACE FUNCTION market_offers_end_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT OLD.active OR NEW.active
       OR (NEW.id, NEW.listing_id, NEW.kind, NEW.licence, NEW.price_usd_micros, NEW.period_days, NEW.included_uses, NEW.seats,
           NEW.trial_uses, NEW.created_at)
          IS DISTINCT FROM
          (OLD.id, OLD.listing_id, OLD.kind, OLD.licence, OLD.price_usd_micros, OLD.period_days, OLD.included_uses, OLD.seats,
           OLD.trial_uses, OLD.created_at) THEN
        RAISE EXCEPTION 'market offer %: an offer is only ever ended, once', OLD.id USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;
CREATE OR REPLACE TRIGGER market_offers_end_only BEFORE UPDATE ON market_offers
    FOR EACH ROW EXECUTE FUNCTION market_offers_end_only();

-- Every listing with a price is sold per use under a commercial licence at that price, as it was: µLXC ÷ 10 is µUSD at
-- the LXC peg ($0.10). A price that is not a whole µUSD (under ten µLXC off one) rounds up to the next, so no paid
-- listing becomes free; Publish takes only whole µUSD from now on.
INSERT INTO market_offers (id, listing_id, kind, licence, price_usd_micros)
SELECT 'ofr_' || gen_random_uuid()::text, l.id, 'per_use', 'commercial', (l.price_per_use_ulxc + 9) / 10
FROM market_listings l
WHERE l.price_per_use_ulxc > 0
  AND NOT EXISTS (SELECT 1 FROM market_offers o WHERE o.listing_id = l.id);
