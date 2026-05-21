CREATE TABLE IF NOT EXISTS api_keys (
  id           TEXT PRIMARY KEY,
  key_hash     TEXT UNIQUE NOT NULL,
  workspace_id TEXT NOT NULL DEFAULT 'default',
  team         TEXT NOT NULL DEFAULT '',
  name         TEXT NOT NULL DEFAULT '',
  active       BOOLEAN NOT NULL DEFAULT true,
  created_at   TIMESTAMPTZ DEFAULT NOW(),
  last_used_at TIMESTAMPTZ,
  expires_at   TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_api_keys_hash
  ON api_keys(key_hash) WHERE active = true;
