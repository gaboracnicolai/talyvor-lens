-- B30.7 — transaction monitoring that opens cases (internal/monitoring).
--
-- Rules run on every payment in or out through a partner and nightly. A hit is a compliance_alerts row — which rule,
-- which payment it judged, every payment the pattern is made of, and a sentence saying why — on the workspace's
-- open monitoring case: the first hit opens the case, later ones extend it. A hit never moves or stops money.
--
-- compliance_cases (0232) takes the monitoring kind: about the workspace, open until an operator decides it (B30.8).
-- Screening's one-case-per-subject key now holds for screening cases only, and a workspace has at most one open
-- monitoring case at a time, so a closed one leaves room for the next.

ALTER TABLE compliance_cases DROP CONSTRAINT IF EXISTS compliance_cases_kind_check;
ALTER TABLE compliance_cases ADD CONSTRAINT compliance_cases_kind_check CHECK (kind IN ('screening', 'monitoring'));
ALTER TABLE compliance_cases DROP CONSTRAINT IF EXISTS compliance_cases_subject_kind_check;
ALTER TABLE compliance_cases ADD CONSTRAINT compliance_cases_subject_kind_check
    CHECK (subject_kind IN ('payee', 'payment', 'workspace'));
ALTER TABLE compliance_cases DROP CONSTRAINT IF EXISTS compliance_cases_outcome_check;
ALTER TABLE compliance_cases ADD CONSTRAINT compliance_cases_outcome_check CHECK (outcome IN ('hit', 'review', 'alert'));
ALTER TABLE compliance_cases DROP CONSTRAINT IF EXISTS compliance_cases_status_check;
ALTER TABLE compliance_cases ADD CONSTRAINT compliance_cases_status_check
    CHECK (status IN ('blocked', 'held', 'released', 'refused', 'open'));

ALTER TABLE compliance_cases DROP CONSTRAINT IF EXISTS compliance_cases_workspace_id_subject_kind_subject_id_name__key;
CREATE UNIQUE INDEX IF NOT EXISTS idx_compliance_cases_screening_subject
    ON compliance_cases (workspace_id, subject_kind, subject_id, name, direction, currency) WHERE kind = 'screening';
CREATE UNIQUE INDEX IF NOT EXISTS idx_compliance_cases_monitoring_open
    ON compliance_cases (workspace_id) WHERE kind = 'monitoring' AND status = 'open';

CREATE TABLE IF NOT EXISTS compliance_alerts (
    id           TEXT PRIMARY KEY CHECK (id <> ''),
    case_id      TEXT NOT NULL REFERENCES compliance_cases (id),
    workspace_id TEXT NOT NULL CHECK (workspace_id <> ''),
    rule         TEXT NOT NULL CHECK (rule <> ''),
    agent_id     TEXT NOT NULL DEFAULT '',          -- the agent whose money moved; '' for the company's own
    entry_id     TEXT NOT NULL CHECK (entry_id <> ''), -- the payment the rule judged (money_entries.id)
    currency     TEXT NOT NULL CHECK (currency <> ''),
    funding      TEXT NOT NULL CHECK (funding IN ('test', 'live')),
    summary      TEXT NOT NULL CHECK (summary <> ''),
    entries      TEXT[] NOT NULL DEFAULT '{}',       -- every payment the pattern is made of, oldest first
    raised_by    TEXT NOT NULL CHECK (raised_by IN ('movement', 'nightly')),
    raised_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A rule judges a payment once: the nightly run finds what the payment itself already raised, and adds nothing.
    UNIQUE (workspace_id, rule, entry_id, currency)
);
CREATE INDEX IF NOT EXISTS idx_compliance_alerts_case ON compliance_alerts (case_id, raised_at);
CREATE INDEX IF NOT EXISTS idx_compliance_alerts_raised ON compliance_alerts (raised_at DESC);

-- The rules read a workspace's payments by time.
CREATE INDEX IF NOT EXISTS idx_money_entries_workspace_created ON money_entries (workspace_id, created_at);
