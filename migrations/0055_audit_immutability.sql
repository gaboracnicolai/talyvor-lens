-- 0055_audit_immutability.sql — U14: append-only enforcement on the audit trail.
--
-- A BEFORE ROW trigger on a partitioned PARENT is automatically created on every
-- partition (current and future), so UPDATE/DELETE are blocked whether issued
-- against the parent OR directly against a partition (empirically verified, pg16).
-- TRUNCATE is statement-level and does NOT cascade to partitions, so its guard is
-- attached to the parent AND each partition; the hash modulus is fixed at 8, so
-- the partition set is CLOSED.
--
-- ⚠ REPARTITIONING CONSTRAINT: the TRUNCATE guards below are enumerated over the
-- partitions that exist AT MIGRATION TIME (via pg_inherits). If a future migration
-- ever repartitions token_events / lens_token_ledger / lxc_ledger (new partitions,
-- different modulus, range instead of hash), the audit_no_truncate triggers MUST be
-- re-applied to the new partition set — the row-level audit_no_mutation trigger
-- re-cascades automatically, but TRUNCATE triggers do NOT. (Tracked in
-- docs/AUDIT-POLICY.md.)
--
-- Guarded (append-only): token_events, lens_token_ledger, lxc_ledger,
-- request_attribution, povi_receipts. NOT guarded (legitimate state transitions):
-- lxc_purchases (refund mark — billing.go) and pool_royalty_mints
-- (held→final/revoked CAS — poolroyalty sweeper/revoker).
--
-- SCOPED RETENTION BYPASS: token_events is BOTH append-only AND the retention
-- target, so a DELETE on token_events is permitted ONLY inside a transaction that
-- sets `SET LOCAL lens.audit_retention = 'on'` — which only the token_events
-- retention sweeper (internal/audit/retention.go) does. UPDATE and TRUNCATE remain
-- ALWAYS blocked (including on token_events); the ledgers (lens_token_ledger,
-- lxc_ledger) are NEVER deletable (GetTotalSupply sums their full history). A stray
-- DELETE (app bug / injection) without the flag is still blocked.
--
-- Threat model: blocks application bugs, accidental mutation, and SQL-injection
-- that does UPDATE/DELETE/TRUNCATE. Does NOT stop a compromised DB role (it can
-- DISABLE/DROP the trigger, or SET the retention flag) — that requires role
-- separation (tracked: U14 follow-up issue "DB role separation"). See
-- docs/AUDIT-POLICY.md.
--
-- Idempotent: CREATE OR REPLACE FUNCTION/TRIGGER (re-run safe).

CREATE OR REPLACE FUNCTION audit_block_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    -- Sanctioned retention delete: token_events (or a partition) under the scoped
    -- flag. Everything else — all UPDATE, all TRUNCATE, every non-flagged DELETE,
    -- and every ledger/attribution/receipt mutation — raises.
    IF TG_OP = 'DELETE' AND TG_TABLE_NAME LIKE 'token_events%'
       AND current_setting('lens.audit_retention', true) = 'on' THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'audit trail is append-only: % on % is blocked (U14)', TG_OP, TG_TABLE_NAME;
END;
$$;

DO $$
DECLARE
    guarded text[] := ARRAY['token_events', 'lens_token_ledger', 'lxc_ledger',
                            'request_attribution', 'povi_receipts'];
    tbl  text;
    part text;
BEGIN
    FOREACH tbl IN ARRAY guarded LOOP
        -- UPDATE/DELETE: row trigger on the parent auto-cascades to all partitions.
        EXECUTE format(
            'CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON %I '
            'FOR EACH ROW EXECUTE FUNCTION audit_block_mutation()', tbl);
        -- TRUNCATE: statement trigger does NOT cascade — attach to parent + each partition.
        EXECUTE format(
            'CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON %I '
            'FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation()', tbl);
        FOR part IN
            SELECT inhrelid::regclass::text FROM pg_inherits WHERE inhparent = tbl::regclass
        LOOP
            EXECUTE format(
                'CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON %s '
                'FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation()', part);
        END LOOP;
    END LOOP;
END;
$$;
