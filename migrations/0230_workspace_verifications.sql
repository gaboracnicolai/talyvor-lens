-- B30.4 — verification levels for people and companies, and the level each capability needs for live money.
--
--   L0  signed in
--   L1  email and phone confirmed
--   L2  identity checked
--   L3  company checked: its number, directors and people with significant control
--
-- Each check past L0 goes to partners.KYCProvider. workspace_verifications is the owner's record of them: the level,
-- the method (the provider that checked: "test" for the Test provider), the status, the date and the provider's
-- reference as evidence — never a document. It is append-only: a check's later status (a pending check passing, a
-- pass withdrawn) is a new row under the same evidence_ref, and the newest row for a reference is where it stands.
-- verified_name, country and company_number are what the check confirmed: the person's or the company's.
--
-- verification_level_limits is the most one movement of live money may move at a level, per currency, in minor
-- units — values Nicolai sets (`lens verification-limits`). It is append-only too: the newest row for a level and
-- currency is in force. Until one is set no level takes live money.

CREATE TABLE IF NOT EXISTS workspace_verifications (
    id             BIGSERIAL PRIMARY KEY,
    workspace_id   TEXT NOT NULL CHECK (workspace_id <> ''),
    level          SMALLINT NOT NULL CHECK (level BETWEEN 1 AND 3),
    subject        TEXT NOT NULL CHECK (subject IN ('contact', 'person', 'company')),
    method         TEXT NOT NULL CHECK (method <> ''),
    status         TEXT NOT NULL CHECK (status IN ('pending', 'completed', 'failed', 'returned')),
    evidence_ref   TEXT NOT NULL CHECK (evidence_ref <> ''),
    verified_name  TEXT NOT NULL DEFAULT '',
    country        TEXT NOT NULL DEFAULT '',
    company_number TEXT NOT NULL DEFAULT '',
    detail         TEXT NOT NULL DEFAULT '',
    requested_by   TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_workspace_verifications_workspace ON workspace_verifications (workspace_id, evidence_ref, id DESC);

CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON workspace_verifications
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON workspace_verifications
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();

CREATE TABLE IF NOT EXISTS verification_level_limits (
    id          BIGSERIAL PRIMARY KEY,
    level       SMALLINT NOT NULL CHECK (level BETWEEN 1 AND 3),
    currency    TEXT NOT NULL CHECK (currency IN ('GBP', 'EUR', 'USD', 'USDC')),
    limit_minor BIGINT NOT NULL CHECK (limit_minor >= 0),
    operator    TEXT NOT NULL CHECK (operator <> ''),
    reference   TEXT NOT NULL CHECK (reference <> ''),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_verification_level_limits ON verification_level_limits (level, currency, id DESC);

CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON verification_level_limits
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON verification_level_limits
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();
