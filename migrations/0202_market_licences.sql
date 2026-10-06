-- B32.19 — licences: buying, renting or subscribing covers the buyer's uses, with version pinning.
--
-- A licence is what a buyer workspace holds once it buys, rents or subscribes to one of a listing's offers (0201), or
-- wins a room prize (B32.33). Buying it is one billed market_uses row (use_kind the offer's kind, at the offer's price)
-- on the buyer's monthly marketplace bill, cleared and earned like any use; while the licence is active, each use it
-- covers is a market_uses row charged 'licensed' at 0 and never metered. A rent's or a subscription's included uses
-- are its offer's (an offer's terms never change); past them the listing's per_use offer bills. pinned_version is the
-- version the licence runs (null: it follows the latest), unless a use names one it may use — any up to the pinned.
--
-- A licence is a money record: only its status, ends_at and auto_renew ever change (an expiry, a renewal, a
-- cancellation, a refund), and it is never deleted.
CREATE TABLE IF NOT EXISTS market_licences (
    id                   TEXT PRIMARY KEY,              -- lic_<uuid>
    listing_id           TEXT NOT NULL,                 -- no foreign key, as on market_uses: a seller who leaves does not take a buyer's licence
    offer_id             TEXT,                          -- the offer it was sold under; null for a prize
    buyer_workspace_id   TEXT NOT NULL,
    agent_id             TEXT NOT NULL DEFAULT '',      -- the buyer's agent whose key bought it
    person_id            TEXT NOT NULL DEFAULT '',      -- the person who bought it: whom a personal licence covers
    licence              TEXT NOT NULL CHECK (licence IN ('personal', 'commercial', 'enterprise')),
    kind                 TEXT NOT NULL CHECK (kind IN ('buy', 'rent', 'subscribe', 'prize')),
    pinned_version       INTEGER CHECK (pinned_version >= 1),
    starts_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    ends_at              TIMESTAMPTZ,                   -- null for a buy or a prize: perpetual
    auto_renew           BOOLEAN NOT NULL DEFAULT false,
    status               TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'expired', 'cancelled', 'refunded', 'unpaid')),
    seats                INTEGER CHECK (seats >= 1),    -- enterprise: the people it covers
    rent_paid_usd_micros BIGINT NOT NULL DEFAULT 0 CHECK (rent_paid_usd_micros >= 0),
    idempotency_key      TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((licence = 'enterprise') = (seats IS NOT NULL)),
    CHECK (licence <> 'personal' OR (agent_id = '' AND person_id <> '')),
    CHECK (ends_at IS NULL OR ends_at > starts_at)
);
CREATE INDEX IF NOT EXISTS idx_market_licences_buyer ON market_licences (buyer_workspace_id, listing_id) WHERE status = 'active';
CREATE UNIQUE INDEX IF NOT EXISTS idx_market_licences_key ON market_licences (buyer_workspace_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

CREATE OR REPLACE FUNCTION market_licences_status_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (to_jsonb(NEW) - 'status' - 'ends_at' - 'auto_renew') = (to_jsonb(OLD) - 'status' - 'ends_at' - 'auto_renew') THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'market licence %: only its status, ends_at and auto_renew may change', OLD.id USING ERRCODE = 'check_violation';
END $$;
CREATE OR REPLACE TRIGGER market_licences_status_only BEFORE UPDATE ON market_licences
    FOR EACH ROW EXECUTE FUNCTION market_licences_status_only();
CREATE OR REPLACE TRIGGER audit_no_delete BEFORE DELETE ON market_licences
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON market_licences
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();

-- What a market_uses row is: a use of the listing, or the purchase, rent, subscription, renewal or prize of a licence
-- to it, or a trial use (B32.21). A use a licence covers is charged 'licensed' (0, never metered); a trial use 'trial'.
-- person_id is who used it, so an enterprise licence counts its people against its seats.
ALTER TABLE market_uses
    ADD COLUMN IF NOT EXISTS use_kind   TEXT NOT NULL DEFAULT 'use'
        CHECK (use_kind IN ('use', 'buy', 'rent', 'subscribe', 'renewal', 'prize', 'trial')),
    ADD COLUMN IF NOT EXISTS licence_id TEXT,
    ADD COLUMN IF NOT EXISTS person_id  TEXT NOT NULL DEFAULT '';
ALTER TABLE market_uses DROP CONSTRAINT IF EXISTS market_uses_charge_check;
ALTER TABLE market_uses ADD CONSTRAINT market_uses_charge_check
    CHECK (charge IN ('billed', 'free', 'own', 'linked', 'licensed', 'trial'));
ALTER TABLE market_uses DROP CONSTRAINT IF EXISTS market_uses_licensed_check;
ALTER TABLE market_uses ADD CONSTRAINT market_uses_licensed_check CHECK (charge <> 'licensed' OR licence_id IS NOT NULL);
CREATE INDEX IF NOT EXISTS idx_market_uses_licence ON market_uses (licence_id) WHERE licence_id IS NOT NULL;
