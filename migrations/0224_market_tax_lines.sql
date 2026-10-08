-- B32.39 — the tax on every billed marketplace use, worked out once, when the use is metered.
--
-- Talyvor is the seller of record on the marketplace, so a billed use — a use, a buy, a rent, a subscription or
-- renewal, a room run, a prize — carries its buyer's tax on top of its price. internal/market asks the tax partner
-- (B32.37) for the buyer's resolved profile (B32.38) and writes its answer here with the evidence it was decided on.
-- A line is never changed: a refund credits its tax back and the journal reverses its posting, but the line stays
-- what the tax was when the use was sold.
--
-- jurisdiction is the buyer's country, '' when no evidence says where the buyer is (test money only: with a live
-- Stripe key such a charge is refused before it runs). taxable_usd_micros and tax_usd_micros are µUSD, like the
-- marketplace journal. treatment is the partner's (standard, reverse_charge, zero, outside_scope, not_registered,
-- no_rate) or unknown_location; note is what a receipt prints for the line.
CREATE TABLE IF NOT EXISTS market_tax_lines (
    use_id             TEXT PRIMARY KEY,
    jurisdiction       TEXT NOT NULL CHECK (jurisdiction ~ '^([A-Z]{2})?$'),
    rate_bps           INTEGER NOT NULL CHECK (rate_bps BETWEEN 0 AND 10000),
    taxable_usd_micros BIGINT NOT NULL CHECK (taxable_usd_micros >= 0),
    tax_usd_micros     BIGINT NOT NULL CHECK (tax_usd_micros >= 0),
    treatment          TEXT NOT NULL CHECK (treatment <> ''),
    note               TEXT NOT NULL,
    evidence           JSONB NOT NULL,
    partner            TEXT NOT NULL,
    calculated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON market_tax_lines
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON market_tax_lines
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();

-- Clearing a taxed use posts its tax to the journal account of the tax authority it is owed to: tax:<jurisdiction>.
ALTER TABLE market_journal_postings DROP CONSTRAINT IF EXISTS market_journal_postings_account_check;
ALTER TABLE market_journal_postings ADD CONSTRAINT market_journal_postings_account_check
    CHECK (account IN ('stripe:clearing', 'revenue:market_fee', 'rounding:stripe', 'stripe:connect_fees', 'credits:issued',
                       'trial:waived', 'trial:market_fee')
           OR account ~ '^seller:.+:(holdback|available)$'
           OR account ~ '^trial:seller:.+$'
           OR account ~ '^tax:[A-Z]{2}$');

-- The tax price a workspace's marketplace subscription carries an item for (LENS_MARKET_TAX_PRICE_ID), so its tax
-- meter events are invoiced as their own line. NULL: none yet — the first tax metered adds it.
ALTER TABLE market_bills ADD COLUMN IF NOT EXISTS tax_price_id TEXT;

-- When a billed use's tax was put on its bill, or found to be nothing. Its price (metered_at) and its tax are metered
-- separately, so a tax event Stripe refuses never keeps a price it took from clearing; MeterPending bills whichever is
-- missing. Every use metered before tax existed owed none.
ALTER TABLE market_uses ADD COLUMN IF NOT EXISTS tax_metered_at TIMESTAMPTZ;
UPDATE market_uses SET tax_metered_at = metered_at WHERE metered_at IS NOT NULL AND tax_metered_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_market_uses_untaxed ON market_uses (used_at)
    WHERE charge = 'billed' AND tax_metered_at IS NULL AND ran_at IS NOT NULL;

-- When a refunded use's tax was credited back on its buyer's bill (CreditMarketRefundTax), so a retried pass credits it
-- once; recorded, like its price's credit, while the refund is not yet credited.
ALTER TABLE market_refunds ADD COLUMN IF NOT EXISTS tax_credited_at TIMESTAMPTZ;
CREATE OR REPLACE FUNCTION market_refunds_credit_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.credited_at IS NULL
       AND (to_jsonb(NEW) - 'stripe_credit_id' - 'credited_at' - 'tax_credited_at')
         = (to_jsonb(OLD) - 'stripe_credit_id' - 'credited_at' - 'tax_credited_at') THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'market_refunds is append-only: only a refund''s Stripe credit may be recorded on it';
END $$;
