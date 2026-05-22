CREATE TABLE IF NOT EXISTS sessions (
  id                  TEXT PRIMARY KEY,
  workspace_id        TEXT NOT NULL DEFAULT 'default',
  agent_name          TEXT NOT NULL DEFAULT 'default',
  turn_count          INTEGER NOT NULL DEFAULT 0,
  total_input_tokens  INTEGER NOT NULL DEFAULT 0,
  total_output_tokens INTEGER NOT NULL DEFAULT 0,
  total_cost_usd      FLOAT NOT NULL DEFAULT 0,
  cache_hits          INTEGER NOT NULL DEFAULT 0,
  cache_misses        INTEGER NOT NULL DEFAULT 0,
  started_at          TIMESTAMPTZ DEFAULT NOW(),
  last_active_at      TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS session_turns (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  session_id    TEXT NOT NULL REFERENCES sessions(id),
  turn_number   INTEGER NOT NULL,
  role          TEXT NOT NULL,
  model         TEXT NOT NULL DEFAULT '',
  input_tokens  INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  cost_usd      FLOAT NOT NULL DEFAULT 0,
  cached        BOOLEAN NOT NULL DEFAULT false,
  created_at    TIMESTAMPTZ DEFAULT NOW()
);
-- Note: prompt/response text is intentionally NOT stored in DB (privacy).
-- Memory-only retention applies for the lifetime of the session.

CREATE INDEX IF NOT EXISTS idx_sessions_workspace
  ON sessions(workspace_id, last_active_at DESC);

CREATE INDEX IF NOT EXISTS idx_turns_session
  ON session_turns(session_id, turn_number);
