-- B37.5 — every call through a partner rail leaves one audit row.
--
-- One row per call: which service and method, for which workspace ('' for an operator's or Lens's own call), under
-- which of the caller's idempotency ids ('' for a read), how it ended and how long it took. It holds no partner
-- reference, account number, name or error text.

CREATE TABLE IF NOT EXISTS partner_calls (
    id             BIGSERIAL PRIMARY KEY,
    at             TIMESTAMPTZ NOT NULL,
    service        TEXT NOT NULL,
    method         TEXT NOT NULL,
    workspace_id   TEXT NOT NULL DEFAULT '',
    idempotency_id TEXT NOT NULL DEFAULT '',
    outcome        TEXT NOT NULL CHECK (outcome IN ('ok', 'refused', 'failed')),
    duration_us    BIGINT NOT NULL CHECK (duration_us >= 0)
);
CREATE INDEX IF NOT EXISTS idx_partner_calls_workspace ON partner_calls (workspace_id, at DESC);

-- An audit row is a record of what was called: it is never changed.
CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON partner_calls
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON partner_calls
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();
