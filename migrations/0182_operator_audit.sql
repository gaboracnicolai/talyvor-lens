-- 0182_operator_audit.sql — B27.28: the operator audit trail.
--
-- One row per action an operator took in the web app: who did it, what they did, what they did it
-- to, and when. Written only through POST /v1/admin/operator-audit/record (the global admin key, or
-- the web app's moderator key naming the operator), read back by operators with filters and as CSV
-- (internal/operatoraudit, cmd/lens/operator_audit_handler.go).
--
-- APPEND-ONLY AT THE DATABASE LEVEL, twice over:
--   * 0055's audit_block_mutation trigger refuses every UPDATE, DELETE and TRUNCATE. This is the
--     guarantee that holds even when the app connects as a superuser — triggers fire for one.
--     audit_block_mutation's retention bypass names token_events only, so it never applies here.
--   * The migrating role (the app's role) loses UPDATE, DELETE and TRUNCATE on the table, so a
--     non-superuser app role is refused before the trigger is reached.
--
-- No workspace_id column: an operator action is cross-tenant, and the target names what it touched.
--
-- Idempotent: IF NOT EXISTS / CREATE OR REPLACE / REVOKE are all re-run safe.

CREATE TABLE IF NOT EXISTS operator_audit (
    id          BIGSERIAL   PRIMARY KEY,
    actor       TEXT        NOT NULL CHECK (btrim(actor) <> ''),
    action      TEXT        NOT NULL CHECK (btrim(action) <> ''),
    target      TEXT        NOT NULL DEFAULT '',
    detail      TEXT        NOT NULL DEFAULT '',
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS operator_audit_occurred_idx ON operator_audit (occurred_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS operator_audit_actor_idx ON operator_audit (actor, occurred_at DESC);
CREATE INDEX IF NOT EXISTS operator_audit_action_idx ON operator_audit (action, occurred_at DESC);

CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON operator_audit
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON operator_audit
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();

REVOKE UPDATE, DELETE, TRUNCATE ON operator_audit FROM PUBLIC;
REVOKE UPDATE, DELETE, TRUNCATE ON operator_audit FROM CURRENT_USER;
