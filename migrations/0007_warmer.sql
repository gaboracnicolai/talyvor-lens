ALTER TABLE token_events
  ADD COLUMN IF NOT EXISTS prompt_text TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_token_events_prompt_hash2
  ON token_events(prompt_hash, created_at DESC)
  WHERE prompt_text != '';
