-- B30.8 — the compliance case file: review, freeze, decide, export (internal/compliance).
--
-- An operator reads a case with its alerts, the owner and agents involved, its notes and a timeline; freezes or
-- unfreezes the workspace's money capabilities; closes the case with a reason; and exports a report draft as text
-- for a person to file. Every action is an operator_audit row whose target is the case (compliance_case:<id>): a
-- note is one too, so the case's notes and timeline are read from the append-only trail, never from a table an
-- UPDATE could rewrite.
--
-- compliance_freezes is each workspace frozen now, and the case it was frozen on: while a row stands every AMBER and
-- RED capability refuses the workspace's money, test or live, and the GREEN ones — Talyvor's own services — go on.
-- An unfreeze deletes the row; the trail keeps when it stood and who froze and unfroze it.

ALTER TABLE compliance_cases DROP CONSTRAINT IF EXISTS compliance_cases_status_check;
ALTER TABLE compliance_cases ADD CONSTRAINT compliance_cases_status_check
    CHECK (status IN ('blocked', 'held', 'released', 'refused', 'open', 'closed'));

CREATE TABLE IF NOT EXISTS compliance_freezes (
    workspace_id TEXT PRIMARY KEY CHECK (workspace_id <> ''),
    case_id      TEXT NOT NULL REFERENCES compliance_cases (id),
    reason       TEXT NOT NULL CHECK (reason <> ''),
    frozen_by    TEXT NOT NULL CHECK (frozen_by <> ''),
    frozen_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A case's timeline reads the trail by target.
CREATE INDEX IF NOT EXISTS operator_audit_target_idx ON operator_audit (target, occurred_at, id);

-- Each 30-second step whose step-up code an operator used (internal/stepup): a code opens one action, once, on any
-- replica. One row per step at most — under three thousand a day.
CREATE TABLE IF NOT EXISTS operator_step_up_codes (
    step     BIGINT PRIMARY KEY,
    operator TEXT NOT NULL CHECK (operator <> ''),
    used_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
