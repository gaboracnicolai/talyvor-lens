-- 0019_token_ledger.sql — foundation for Lens Batch 2 (token mining).
--
-- Two tables:
--   lens_token_ledger    — append-only history of every credit + debit
--   lens_token_balances  — denormalised current balance + lifetime totals
--
-- The ledger is the source of truth — balances are recomputable from
-- the ledger if they ever drift. We materialise the balance because
-- /v1/workspaces/:wsID/tokens/balance is read on every dashboard load
-- and summing N ledger rows would scale poorly.

CREATE TABLE IF NOT EXISTS lens_token_ledger (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  TEXT NOT NULL,
    amount        DOUBLE PRECISION NOT NULL,
    balance_after DOUBLE PRECISION NOT NULL,
    type          TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    metadata      JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_ledger_workspace
    ON lens_token_ledger (workspace_id, created_at DESC);

-- Type index helps the mining-stats endpoint scan only cache_mine rows.
CREATE INDEX IF NOT EXISTS idx_ledger_type
    ON lens_token_ledger (type, workspace_id);

CREATE TABLE IF NOT EXISTS lens_token_balances (
    workspace_id    TEXT PRIMARY KEY,
    balance         DOUBLE PRECISION NOT NULL DEFAULT 0,
    lifetime_earned DOUBLE PRECISION NOT NULL DEFAULT 0,
    lifetime_spent  DOUBLE PRECISION NOT NULL DEFAULT 0,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
