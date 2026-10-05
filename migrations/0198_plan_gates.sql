-- B32.12 — what each plan unlocks (internal/plans, LENS_PLAN_GATES). An Enterprise contract may carry its own
-- limits: agents and seats the workspace may have (-1 unlimited); NULL follows enterprise's gates.
ALTER TABLE workspace_contracts ADD COLUMN IF NOT EXISTS agents BIGINT CHECK (agents >= -1);
ALTER TABLE workspace_contracts ADD COLUMN IF NOT EXISTS seats BIGINT CHECK (seats >= -1);
