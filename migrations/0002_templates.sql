CREATE TABLE IF NOT EXISTS prompt_templates (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  hash        TEXT UNIQUE NOT NULL,
  content     TEXT NOT NULL,
  provider    TEXT NOT NULL,
  token_count INTEGER NOT NULL DEFAULT 0,
  hit_count   INTEGER NOT NULL DEFAULT 0,
  pinned_at   TIMESTAMPTZ,
  created_at  TIMESTAMPTZ DEFAULT NOW(),
  updated_at  TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_templates_hash
  ON prompt_templates(hash);

CREATE INDEX IF NOT EXISTS idx_templates_hit_count
  ON prompt_templates(hit_count DESC)
  WHERE pinned_at IS NULL;
