CREATE TABLE IF NOT EXISTS workspaces (
  id                     TEXT PRIMARY KEY,
  name                   TEXT NOT NULL,
  cache_prefix           TEXT NOT NULL,
  spend_limit_usd        FLOAT NOT NULL DEFAULT 0,
  allowed_models         TEXT[] NOT NULL DEFAULT '{}',
  allowed_providers      TEXT[] NOT NULL DEFAULT '{}',
  max_tokens_per_request INTEGER NOT NULL DEFAULT 0,
  active                 BOOLEAN NOT NULL DEFAULT true,
  created_at             TIMESTAMPTZ DEFAULT NOW(),
  updated_at             TIMESTAMPTZ DEFAULT NOW()
);

ALTER TABLE token_events
  ADD COLUMN IF NOT EXISTS workspace_id TEXT NOT NULL DEFAULT 'default';

CREATE INDEX IF NOT EXISTS idx_token_events_workspace
  ON token_events(workspace_id, created_at DESC);

ALTER TABLE workspaces
  ADD COLUMN IF NOT EXISTS max_output_tokens INTEGER NOT NULL DEFAULT 0;

ALTER TABLE workspaces
  ADD COLUMN IF NOT EXISTS max_input_tokens INTEGER NOT NULL DEFAULT 0;
