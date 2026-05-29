-- 0028_budgets.sql — per-team / per-sprint budget governance
-- (Master Plan Upgrade 19, Lens portion).
--
-- Extends the existing per-workspace spend tracking into budgets
-- scoped to workspace / team / sprint, with off / alert / hard_block
-- enforcement and a 50/80/90%-style alert-threshold ladder.
--
-- ─── token_events: sprint_id + the workspace_id CORRECTNESS FIX ───
--
-- token_events already carries `team` and `workspace_id`. This adds
-- `sprint_id` so per-sprint spend can be summed from the same single
-- write path the billing pipeline already uses (alerts.RecordSpend).
--
-- CORRECTNESS FIX (not a feature): alerts.RecordSpend historically
-- never wrote workspace_id, so every billing row fell back to the
-- column default 'default'. The per-workspace spend cap
-- (workspace.Manager.CheckPolicy → SUM(cost_usd) WHERE workspace_id=$1)
-- was therefore summing ALL spend under 'default' rather than per
-- workspace — its original intent. RecordSpend now writes the real
-- workspace_id, so both the cap and the new workspace-scoped budgets
-- aggregate from one source of truth.
--
-- HISTORICAL ROWS ARE NOT RETROACTIVELY REASSIGNED: rows written
-- before this change keep workspace_id='default'. The true workspace
-- for a historical row is unknowable, so we deliberately leave old
-- rows as-is; only rows written after this change carry real ids.
-- (Safe here: no production data exists yet.)

ALTER TABLE token_events
  ADD COLUMN IF NOT EXISTS sprint_id TEXT NOT NULL DEFAULT '';

-- Reconciliation reads spend by (workspace_id, team, sprint_id) over a
-- time window; this composite index keeps those SUMs cheap.
CREATE INDEX IF NOT EXISTS idx_token_events_budget_scope
  ON token_events(workspace_id, team, sprint_id, created_at DESC);

-- ─── budgets ───
--
-- One row per budget. scope_id is the concrete identifier within the
-- scope: the workspace id for scope='workspace', the team id for
-- scope='team', the sprint id for scope='sprint'.
--
-- spent_usd is a cached running total maintained by the budget service
-- (incremented live on the recording path, reconciled from token_events
-- on refresh). token_events remains the source of truth; spent_usd is a
-- convenience snapshot for the API/dashboard and survives restarts.

CREATE TABLE IF NOT EXISTS budgets (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id     TEXT NOT NULL,
    scope            TEXT NOT NULL
        CHECK (scope IN ('workspace', 'team', 'sprint')),
    scope_id         TEXT NOT NULL DEFAULT '',
    period           TEXT NOT NULL DEFAULT 'monthly'
        CHECK (period IN ('monthly', 'weekly', 'total')),
    limit_usd        NUMERIC(12, 4) NOT NULL DEFAULT 0,
    spent_usd        NUMERIC(12, 4) NOT NULL DEFAULT 0,
    -- Fractional thresholds (0..1) at which to fire an alert as spend
    -- climbs toward the limit. Each fires once per period.
    alert_thresholds DOUBLE PRECISION[] NOT NULL
        DEFAULT ARRAY[0.5, 0.8, 0.9]::DOUBLE PRECISION[],
    -- off       — track spend only; never alert, never block.
    -- alert      — track + fire threshold alerts; never block (default).
    -- hard_block — track + alert + reject requests once over the limit.
    enforcement      TEXT NOT NULL DEFAULT 'alert'
        CHECK (enforcement IN ('off', 'alert', 'hard_block')),
    starts_at        TIMESTAMPTZ,
    ends_at          TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- The hot-path scope resolution looks budgets up by
-- (workspace_id, scope, scope_id); one budget per such triple.
CREATE UNIQUE INDEX IF NOT EXISTS idx_budgets_scope
    ON budgets(workspace_id, scope, scope_id);
