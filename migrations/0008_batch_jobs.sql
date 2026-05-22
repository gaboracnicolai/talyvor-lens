CREATE TABLE IF NOT EXISTS batch_jobs (
  id            TEXT PRIMARY KEY,
  request_id    TEXT NOT NULL,
  batch_id      TEXT NOT NULL DEFAULT '',
  workspace_id  TEXT NOT NULL DEFAULT 'default',
  provider      TEXT NOT NULL,
  model         TEXT NOT NULL,
  prompt_hash   TEXT NOT NULL DEFAULT '',
  status        TEXT NOT NULL DEFAULT 'pending',
  response      TEXT,
  cost_usd      FLOAT NOT NULL DEFAULT 0,
  created_at    TIMESTAMPTZ DEFAULT NOW(),
  completed_at  TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_batch_jobs_workspace
  ON batch_jobs(workspace_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_batch_jobs_status
  ON batch_jobs(status) WHERE status != 'complete';
