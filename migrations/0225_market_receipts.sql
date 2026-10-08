-- B32.40 — a VAT receipt for every paid marketplace bill, issued by Talyvor as the supplier.
--
-- Under the deemed-supplier rules Talyvor is the supplier of every marketplace sale, so the receipt for a paid bill is
-- Talyvor's. When a buyer's marketplace invoice is paid (invoice.paid), internal/market issues one receipt for it: the
-- next number of the year, the supplier and buyer as they were at that moment, and one line per use the invoice
-- cleared with its net, rate and tax (from market_tax_lines). A receipt is a tax document: it is never changed.
--
-- Numbers run per series and year with no gaps: market_receipt_sequences is bumped in the transaction that inserts
-- the receipt, so a receipt that fails to insert takes its number back with it. Live money and test money are
-- separate series ('live', 'test'), so a test receipt never takes a number from the live run.
CREATE TABLE IF NOT EXISTS market_receipt_sequences (
    series      TEXT NOT NULL CHECK (series IN ('live', 'test')),
    year        INTEGER NOT NULL CHECK (year BETWEEN 2000 AND 9999),
    last_number INTEGER NOT NULL CHECK (last_number > 0),
    PRIMARY KEY (series, year)
);

-- supplier and buyer are what the receipt prints for each, as they were when it was issued. stripe_total_cents is
-- the paid invoice's total as Stripe reported it (NULL for a test bill Lens paid itself, B25.7). tax_local_* is the
-- receipt's tax in the buyer's currency at its jurisdiction's rate source, NULL when no rate was published yet.
CREATE TABLE IF NOT EXISTS market_receipts (
    id                     TEXT PRIMARY KEY,
    series                 TEXT NOT NULL CHECK (series IN ('live', 'test')),
    year                   INTEGER NOT NULL,
    number                 INTEGER NOT NULL CHECK (number > 0),
    invoice_id             TEXT NOT NULL UNIQUE,
    buyer_workspace_id     TEXT NOT NULL,
    issued_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    paid_at                TIMESTAMPTZ NOT NULL,
    supplier               JSONB NOT NULL,
    buyer                  JSONB NOT NULL,
    stripe_total_cents     BIGINT,
    tax_local_currency     TEXT CHECK (tax_local_currency ~ '^[A-Z]{3}$'),
    tax_local_minor        BIGINT,
    tax_local_rate         TEXT,
    tax_local_rate_date    DATE,
    tax_local_source       TEXT,
    UNIQUE (series, year, number)
);
CREATE INDEX IF NOT EXISTS idx_market_receipts_buyer ON market_receipts (buyer_workspace_id, issued_at DESC);

-- One line per use the paid invoice cleared, in the order it was used. net is the price before tax, µUSD like the
-- marketplace journal; rate_bps, tax, treatment, jurisdiction and note are its tax line's ('' and 0 for a use sold
-- before tax existed).
CREATE TABLE IF NOT EXISTS market_receipt_lines (
    receipt_id     TEXT NOT NULL REFERENCES market_receipts (id),
    position       INTEGER NOT NULL CHECK (position > 0),
    use_id         TEXT NOT NULL UNIQUE,
    description    TEXT NOT NULL,
    net_usd_micros BIGINT NOT NULL CHECK (net_usd_micros >= 0),
    rate_bps       INTEGER NOT NULL CHECK (rate_bps BETWEEN 0 AND 10000),
    tax_usd_micros BIGINT NOT NULL CHECK (tax_usd_micros >= 0),
    treatment      TEXT NOT NULL,
    jurisdiction   TEXT NOT NULL,
    note           TEXT NOT NULL,
    PRIMARY KEY (receipt_id, position)
);

CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON market_receipts
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON market_receipts
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON market_receipt_lines
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON market_receipt_lines
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();
