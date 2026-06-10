CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS prompt_embeddings (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  provider    TEXT NOT NULL,
  model       TEXT NOT NULL,
  prompt_hash TEXT NOT NULL,
  embedding   vector(1536),
  response    TEXT NOT NULL,
  tokens_saved INTEGER NOT NULL DEFAULT 0,
  hit_count   INTEGER NOT NULL DEFAULT 0,
  created_at  TIMESTAMPTZ DEFAULT NOW(),
  updated_at  TIMESTAMPTZ DEFAULT NOW()
);

-- ADD CONSTRAINT has no IF NOT EXISTS; the DO block makes the re-run a no-op
-- (42P07/42710 swallowed) instead of killing a replay at the very first file
-- (#127). Deliberately NOT drop-then-add: that would briefly lose the
-- uniqueness guarantee under concurrent writes and rebuild the index.
DO $$ BEGIN
  ALTER TABLE prompt_embeddings
    ADD CONSTRAINT uq_prompt_hash UNIQUE (prompt_hash);
EXCEPTION WHEN duplicate_table OR duplicate_object THEN NULL;
END $$;

CREATE INDEX IF NOT EXISTS idx_embeddings_vector
  ON prompt_embeddings
  USING ivfflat (embedding vector_cosine_ops)
  WITH (lists = 100);

CREATE TABLE IF NOT EXISTS token_events (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  provider     TEXT NOT NULL,
  model        TEXT NOT NULL,
  input_tokens  INTEGER NOT NULL,
  output_tokens INTEGER NOT NULL,
  cached        BOOLEAN NOT NULL DEFAULT false,
  compressed    BOOLEAN NOT NULL DEFAULT false,
  savings_pct   FLOAT NOT NULL DEFAULT 0,
  team          TEXT,
  feature       TEXT,
  user_id       TEXT,
  created_at    TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_token_events_created
  ON token_events(created_at DESC);

ALTER TABLE token_events
  ADD COLUMN IF NOT EXISTS prompt_hash TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_token_events_prompt_hash
  ON token_events(prompt_hash);

ALTER TABLE token_events
  ADD COLUMN IF NOT EXISTS cost_usd FLOAT NOT NULL DEFAULT 0;

ALTER TABLE token_events
  ADD COLUMN IF NOT EXISTS pii_detected BOOLEAN NOT NULL DEFAULT false;
