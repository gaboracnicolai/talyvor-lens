-- Per-workspace guardrail policies. The engine keeps an in-memory
-- cache; this table provides the persistence layer so policies survive
-- a process restart. JSONB lets us evolve the policy struct without
-- adding/dropping columns each release.
CREATE TABLE IF NOT EXISTS guardrail_policies (
    workspace_id TEXT PRIMARY KEY,
    policy       JSONB NOT NULL,
    created_at   TIMESTAMPTZ DEFAULT NOW(),
    updated_at   TIMESTAMPTZ DEFAULT NOW()
);

-- Audit trail of guardrail events. Prompt content NEVER lands here —
-- only metadata (workspace, violation type, action, risk score).
-- Compliance teams query this for "show me every blocked request"
-- type reports.
CREATE TABLE IF NOT EXISTS guardrail_events (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id   TEXT NOT NULL,
    violation_type TEXT NOT NULL,
    action_taken   TEXT NOT NULL,
    risk_score     FLOAT NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_guardrail_events_workspace
    ON guardrail_events(workspace_id, created_at DESC);
