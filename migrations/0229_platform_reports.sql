-- B32.44 — the annual platform-reporting export (the UK reporting rules and EU DAC7): one row per file written.
--
-- `lens platform-report --year 2026` and POST /v1/admin/platform-reports write a file with one record per
-- reportable seller — resident in the UK or an EU member state — and activity: their identification, the TINs, date
-- of birth and account identifier opened from seller_tax_profiles inside the file only, and per quarter the
-- consideration credited to them, the number of activities, Talyvor's fees and the taxes withheld (none), all from the
-- marketplace journal. The file is not kept here: this row is that it was written, by whom, how many records it had
-- and its sha256, so a file handed to a tax authority can be matched to the run that produced it. Each run is also in
-- the operator audit trail.
--
-- funding is the money reported: live, or test money kept apart. format is the file's: csv or json.

CREATE TABLE IF NOT EXISTS platform_reports (
    id           TEXT PRIMARY KEY,  -- prp_<uuid>
    year         INTEGER NOT NULL CHECK (year BETWEEN 2020 AND 2100),
    funding      TEXT NOT NULL CHECK (funding IN ('test', 'live')),
    format       TEXT NOT NULL CHECK (format IN ('csv', 'json')),
    generated_at TIMESTAMPTZ NOT NULL,
    operator     TEXT NOT NULL CHECK (operator <> ''),
    rows         INTEGER NOT NULL CHECK (rows >= 0),
    sha256       TEXT NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$')
);
CREATE INDEX IF NOT EXISTS idx_platform_reports_year ON platform_reports (year, generated_at DESC);

CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON platform_reports
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON platform_reports
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();
