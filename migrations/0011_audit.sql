-- Audit export needs per-request correlation IDs. token_events already
-- carries every spend/cost field; these two columns let SIEM ingest
-- group records by chat session and pinpoint a specific HTTP request.

ALTER TABLE token_events
  ADD COLUMN IF NOT EXISTS session_id TEXT NOT NULL DEFAULT '';

ALTER TABLE token_events
  ADD COLUMN IF NOT EXISTS request_id TEXT NOT NULL DEFAULT '';

-- Audit-flavoured index: most compliance queries are scoped by
-- workspace and time, sometimes drilled down by team or provider.
-- This composite index sorts created_at DESC inside the index so
-- the typical "last N days for ws-X" report skips a sort.
CREATE INDEX IF NOT EXISTS idx_token_events_audit
  ON token_events(workspace_id, created_at DESC, team, provider);
