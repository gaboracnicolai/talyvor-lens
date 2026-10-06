-- B35.1 — a synthetic (test) workspace on any plan, on test money.
--
-- The testers put a synthetic workspace on free, team, business or enterprise through the synthetic-key routes
-- (POST /v1/synthetic/workspaces {"plan":…}, POST /v1/synthetic/workspaces/{id}/plan). internal/plans reads it
-- after a contract and a paying subscription, so a synthetic workspace that subscribes in Stripe test mode is on
-- what it bought. NULL is free, as every synthetic workspace was before. Only a synthetic workspace may carry
-- one: a real workspace's plan is bought through Stripe or contracted, never set here.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS synthetic_plan TEXT;

ALTER TABLE workspaces DROP CONSTRAINT IF EXISTS workspaces_synthetic_plan_check;
ALTER TABLE workspaces ADD CONSTRAINT workspaces_synthetic_plan_check CHECK (
    synthetic_plan IS NULL OR (synthetic AND synthetic_plan IN ('free', 'team', 'business', 'enterprise')));
