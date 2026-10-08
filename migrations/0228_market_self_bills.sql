-- B32.43 — self-billed invoices: the seller's supply to Talyvor, with their VAT.
--
-- Under the deemed-supplier rules Talyvor buys from the seller and sells to the buyer, so a VAT-registered seller makes
-- a supply to Talyvor. A seller who has agreed to self-billing (seller_tax_profiles.self_billing_agreed_version, B32.41)
-- is sent no invoice to raise: each weekly payout is also a self-billed invoice from the seller to Talyvor for the
-- earnings it pays, taxed through TaxPartner.Calculate with the seller as the supplier. VAT the seller charges is added
-- on top of the payout and posted −VAT to the seller's available balance and +VAT to tax:<XX>:input, the input VAT
-- Talyvor reclaims. While LENS_SELF_BILLING_VAT is off the invoices are issued at zero VAT, under review.
--
-- Numbers run per seller and series with no gaps: the payout run holds the seller's lock while it takes the next one.
-- Live money and test money are separate series, so a test invoice never takes a number from the live run. supplier and
-- customer are what the invoice prints for each, as they were when it was issued. An invoice is never changed.
CREATE TABLE IF NOT EXISTS market_self_bills (
    id                 TEXT PRIMARY KEY,                 -- msb_<uuid>
    payout_id          TEXT NOT NULL UNIQUE REFERENCES market_payouts (id),
    workspace_id       TEXT NOT NULL,                    -- the seller: the supplier
    series             TEXT NOT NULL CHECK (series IN ('live', 'test')),
    number             INTEGER NOT NULL CHECK (number > 0),
    period             TEXT NOT NULL,                    -- the ISO week of the payout, such as 2026-W41
    issued_at          TIMESTAMPTZ NOT NULL,
    agreement_version  TEXT NOT NULL,                    -- the self-billing agreement the seller accepted
    supplier           JSONB NOT NULL,
    customer           JSONB NOT NULL,
    net_usd_micros     BIGINT NOT NULL CHECK (net_usd_micros > 0),
    vat_usd_micros     BIGINT NOT NULL CHECK (vat_usd_micros >= 0),
    rate_bps           INTEGER NOT NULL CHECK (rate_bps BETWEEN 0 AND 10000),
    jurisdiction       TEXT NOT NULL,
    treatment          TEXT NOT NULL,
    note               TEXT NOT NULL,
    partner            TEXT NOT NULL,                    -- the tax partner that answered; '' while VAT is under review
    vat_enabled        BOOLEAN NOT NULL,                 -- LENS_SELF_BILLING_VAT when it was issued
    UNIQUE (workspace_id, series, number)
);
CREATE INDEX IF NOT EXISTS idx_market_self_bills_seller ON market_self_bills (workspace_id, period);

CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON market_self_bills
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON market_self_bills
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();

-- A payout carries the seller's VAT on top of the earnings it pays: gross stays what leaves the seller's earnings, and
-- Stripe's fees and the net are of gross and VAT together.
ALTER TABLE market_payouts ADD COLUMN IF NOT EXISTS vat_usd_micros BIGINT NOT NULL DEFAULT 0 CHECK (vat_usd_micros >= 0);
ALTER TABLE market_payouts DROP CONSTRAINT IF EXISTS market_payouts_check;
ALTER TABLE market_payouts ADD CONSTRAINT market_payouts_check
    CHECK (gross_usd_micros + vat_usd_micros = account_fee_usd_micros + payout_fee_usd_micros + net_usd_micros);

-- The journal posts the seller's VAT as a self_bill entry, to the input VAT Talyvor reclaims: tax:<jurisdiction>:input.
ALTER TABLE market_journal_entries DROP CONSTRAINT IF EXISTS market_journal_entries_kind_check;
ALTER TABLE market_journal_entries ADD CONSTRAINT market_journal_entries_kind_check
    CHECK (kind IN ('clear', 'reversal', 'release', 'payout', 'credits', 'rounding', 'trial', 'self_bill'));
ALTER TABLE market_journal_postings DROP CONSTRAINT IF EXISTS market_journal_postings_account_check;
ALTER TABLE market_journal_postings ADD CONSTRAINT market_journal_postings_account_check
    CHECK (account IN ('stripe:clearing', 'revenue:market_fee', 'rounding:stripe', 'stripe:connect_fees', 'credits:issued',
                       'trial:waived', 'trial:market_fee')
           OR account ~ '^seller:.+:(holdback|available)$'
           OR account ~ '^trial:seller:.+$'
           OR account ~ '^tax:[A-Z]{2}(:input)?$');
