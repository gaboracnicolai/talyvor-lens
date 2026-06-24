-- 0066_reputation_events.sql — the append-only event log that makes annotation reputation
-- REAL and MONEY-DECOUPLED. Reputation = clamp(0.5 + SUM(delta), 0, 1) folded over this log
-- (baseline 0.5; a new annotator starts NEUTRAL, not the max). Events:
--   agreement_outcome — a resolved (TTL-expired) task's FINAL-consensus agreement, ONE per
--                       (annotator, task); idem_key = task_id.
--   decay             — dormancy erosion toward baseline (a follow-up cron); idem_key = decay_date.
--   admin_reset       — operator reset (re-entry); idem_key = a reset id.
-- APPEND-ONLY: the writer only INSERTs ON CONFLICT (annotator_id, kind, idem_key) DO NOTHING
-- (re-runs never double-count), AND a BEFORE UPDATE/DELETE trigger makes the table immutable at
-- the DB level (the U14 audit-trail pattern) — so a reputation value's provenance is provable,
-- which matters if reputation ever couples to money.
--
-- HARD BOUNDARY: reputation NEVER feeds the earning/mint path. annotation_mining.go:330-355
-- stays base + high-agreement bonus, reputation-free — pinned by an AST money-guard test.

CREATE TABLE IF NOT EXISTS reputation_events (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    annotator_id TEXT             NOT NULL,
    kind         TEXT             NOT NULL,   -- 'agreement_outcome' | 'decay' | 'admin_reset'
    idem_key     TEXT             NOT NULL,   -- task_id (outcome) | decay_date (decay) | reset_id
    delta        DOUBLE PRECISION NOT NULL,   -- signed change to the score
    reason       JSONB            NOT NULL,   -- {task_id, agreement, majority_fraction, others, diversity} | {dormant_days} | {by}
    created_at   TIMESTAMPTZ      NOT NULL DEFAULT now(),
    UNIQUE (annotator_id, kind, idem_key)
);

CREATE INDEX IF NOT EXISTS idx_reputation_events_annotator ON reputation_events (annotator_id);

-- Append-only at the DB level (U14 pattern): block every UPDATE/DELETE. Idempotent (re-run safe).
CREATE OR REPLACE FUNCTION reputation_events_block_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'reputation_events is append-only: % is blocked', TG_OP;
END;
$$;

CREATE OR REPLACE TRIGGER reputation_events_no_mutation
    BEFORE UPDATE OR DELETE ON reputation_events
    FOR EACH ROW EXECUTE FUNCTION reputation_events_block_mutation();
