-- B20.4 — marketplace safety: review, reporting, takedown.
--
-- review_status says whether anyone but a listing's owner may find and use it:
--   approved    the automatic review passed (every listing published before B20.4 passed B20.1's scan)
--   held        the automatic review found something a person must look at (internal/market/review.go);
--               only its owner sees it, until an admin approves it
--   taken_down  an admin took it down: nobody may use it, and its uses inside the holdback were refunded
ALTER TABLE market_listings
    ADD COLUMN IF NOT EXISTS review_status TEXT NOT NULL DEFAULT 'approved'
        CHECK (review_status IN ('approved', 'held', 'taken_down')),
    ADD COLUMN IF NOT EXISTS review_reason TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS taken_down_at TIMESTAMPTZ;

-- Anyone may report a listing they can see; one open report per reporter per listing. An admin's decision
-- (take it down, or keep it up) resolves every open report on it.
CREATE TABLE IF NOT EXISTS market_listing_reports (
    id                    TEXT PRIMARY KEY,
    listing_id            TEXT NOT NULL REFERENCES market_listings (id) ON DELETE CASCADE,
    reporter_workspace_id TEXT NOT NULL,
    reason                TEXT NOT NULL CHECK (reason IN ('malicious', 'injection', 'secret', 'personal_data', 'infringing', 'misleading', 'other')),
    details               TEXT NOT NULL DEFAULT '',
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at           TIMESTAMPTZ,
    resolution            TEXT CHECK (resolution IN ('taken_down', 'kept')),
    CHECK ((resolved_at IS NULL) = (resolution IS NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_market_listing_reports_open
    ON market_listing_reports (listing_id, reporter_workspace_id) WHERE resolved_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_market_listing_reports_reporter ON market_listing_reports (reporter_workspace_id);

-- One refund per billed use of a taken-down listing, append-only like the earnings it reverses. The buyer
-- is credited the use's price as a negative line on their next marketplace invoice; a use that had
-- cleared has its seller's share reversed here, and SellerEarnings subtracts it. The only change a row
-- ever takes is Stripe accepting its credit (stripe_credit_id, credited_at); a use that was never metered
-- was never billed, so it is refunded by never being billed and has no credit.
CREATE TABLE IF NOT EXISTS market_refunds (
    use_id                    TEXT PRIMARY KEY,  -- market_uses.id: a use is refunded once
    listing_id                TEXT NOT NULL,
    buyer_workspace_id        TEXT NOT NULL,
    seller_workspace_id       TEXT NOT NULL,
    price_ulxc                BIGINT NOT NULL CHECK (price_ulxc > 0),
    gross_usd_micros          BIGINT NOT NULL CHECK (gross_usd_micros >= 0),          -- what the buyer is given back
    reversed_share_usd_micros BIGINT NOT NULL DEFAULT 0 CHECK (reversed_share_usd_micros >= 0), -- 0: the use had not cleared
    reason                    TEXT NOT NULL,
    refunded_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    stripe_credit_id          TEXT,
    credited_at               TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_market_refunds_seller ON market_refunds (seller_workspace_id);
CREATE INDEX IF NOT EXISTS idx_market_refunds_uncredited ON market_refunds (refunded_at) WHERE credited_at IS NULL;

CREATE OR REPLACE FUNCTION market_refunds_credit_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.credited_at IS NULL
       AND (to_jsonb(NEW) - 'stripe_credit_id' - 'credited_at') = (to_jsonb(OLD) - 'stripe_credit_id' - 'credited_at') THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'market_refunds is append-only: only a refund''s Stripe credit may be recorded on it';
END $$;
CREATE OR REPLACE TRIGGER market_refunds_credit_only BEFORE UPDATE ON market_refunds
    FOR EACH ROW EXECUTE FUNCTION market_refunds_credit_only();
CREATE OR REPLACE TRIGGER audit_no_delete BEFORE DELETE ON market_refunds
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON market_refunds
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();
