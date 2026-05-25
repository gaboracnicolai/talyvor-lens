-- 0018_tenants.sql — multi-tenant workspace isolation tables.
--
-- workspace_configs: per-workspace policy bundle (spend cap,
--   monthly budget, rate limits, model + provider allowlists,
--   log level, retention policy).
--
-- workspace_api_keys: bcrypt-hashed workspace-scoped keys.
--   Plaintext is shown to the admin exactly once at creation;
--   only the hash + first-N-char prefix land here.
--
-- The existing `auth.api_keys` table continues to back the
-- Lens-admin keys; this is purely additive.

CREATE TABLE IF NOT EXISTS workspace_configs (
    id                TEXT PRIMARY KEY,
    name              TEXT NOT NULL DEFAULT '',
    spending_cap      NUMERIC(12, 4) NOT NULL DEFAULT 0,
    monthly_budget    NUMERIC(12, 4) NOT NULL DEFAULT 0,
    rate_limit_rpm    INTEGER NOT NULL DEFAULT 0,
    rate_limit_tpm    INTEGER NOT NULL DEFAULT 0,
    allowed_models    TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[],
    allowed_providers TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[],
    log_level         TEXT NOT NULL DEFAULT 'all'
        CHECK (log_level IN ('all', 'errors', 'none')),
    retention_days    INTEGER NOT NULL DEFAULT 90
        CHECK (retention_days >= 0),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS workspace_api_keys (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  TEXT NOT NULL,
    key_hash      TEXT NOT NULL,
    key_prefix    TEXT NOT NULL,
    name          TEXT NOT NULL DEFAULT '',
    scopes        TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[],
    last_used_at  TIMESTAMPTZ,
    expires_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Prefix index keeps ValidateAPIKey O(matching keys) instead of
-- O(all keys) — bcrypt comparison is expensive enough that we
-- want to bound the candidate set sharply.
CREATE INDEX IF NOT EXISTS workspace_api_keys_prefix_idx
    ON workspace_api_keys (key_prefix);

-- Admin UI lists keys per workspace, newest first.
CREATE INDEX IF NOT EXISTS workspace_api_keys_workspace_idx
    ON workspace_api_keys (workspace_id, created_at DESC);
