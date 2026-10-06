-- B32.20 — subscriptions renew and cancel at period end, and rentals add up to ownership.
--
-- On the schedules' tick each active licence that auto-renews is renewed once its ends_at has come: one market_uses row
-- (use_kind 'renewal', at its offer's price) per period, stamped at the period's start — at most one per licence and
-- period start (idx_market_uses_renewal). A refused renewal, or a bill Stripe gives up on, leaves the licence 'unpaid':
-- it runs to its ends_at and renews no more. A cancel turns auto_renew off.
--
-- Rent-to-own: each rent, or renewal of a rent, that clears adds its price to its licence's rent_paid_usd_micros, which
-- therefore counts what cleared and only ever rises. Once a buyer's rents of a listing under one licence type have paid
-- that listing's buy offer for the same licence type, a perpetual licence (kind buy, source rent_to_own) is issued at
-- no charge — once per buyer, listing, licence type and person (idx_market_licences_rent_to_own).
ALTER TABLE market_licences ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'offer' CHECK (source IN ('offer', 'rent_to_own'));
CREATE UNIQUE INDEX IF NOT EXISTS idx_market_licences_rent_to_own ON market_licences (buyer_workspace_id, listing_id, licence, person_id)
    WHERE source = 'rent_to_own';
CREATE UNIQUE INDEX IF NOT EXISTS idx_market_uses_renewal ON market_uses (licence_id, used_at) WHERE use_kind = 'renewal';

-- B32.19 recorded a rent's price as paid when it was billed; from here it is what cleared.
CREATE OR REPLACE FUNCTION market_licences_status_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (to_jsonb(NEW) - 'status' - 'ends_at' - 'auto_renew' - 'rent_paid_usd_micros')
     = (to_jsonb(OLD) - 'status' - 'ends_at' - 'auto_renew' - 'rent_paid_usd_micros') THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'market licence %: only its status, ends_at, auto_renew and rent paid may change', OLD.id USING ERRCODE = 'check_violation';
END $$;
UPDATE market_licences c SET rent_paid_usd_micros = COALESCE((SELECT sum(e.gross_usd_micros) FROM market_uses u
        JOIN market_earnings e ON e.use_id = u.id WHERE u.licence_id = c.id AND u.use_kind IN ('rent', 'renewal')), 0)
    WHERE c.kind = 'rent';

CREATE OR REPLACE FUNCTION market_licences_status_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (to_jsonb(NEW) - 'status' - 'ends_at' - 'auto_renew' - 'rent_paid_usd_micros')
     = (to_jsonb(OLD) - 'status' - 'ends_at' - 'auto_renew' - 'rent_paid_usd_micros')
       AND NEW.rent_paid_usd_micros >= OLD.rent_paid_usd_micros THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'market licence %: only its status, ends_at and auto_renew may change, and its rent paid only rise', OLD.id
        USING ERRCODE = 'check_violation';
END $$;
