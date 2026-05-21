CREATE TABLE IF NOT EXISTS branch_spend (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  branch       TEXT NOT NULL DEFAULT '',
  pr_number    TEXT NOT NULL DEFAULT '',
  commit_sha   TEXT NOT NULL DEFAULT '',
  repository   TEXT NOT NULL DEFAULT '',
  team         TEXT NOT NULL DEFAULT '',
  feature      TEXT NOT NULL DEFAULT '',
  model        TEXT NOT NULL,
  input_tokens  INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  cost_usd      FLOAT NOT NULL DEFAULT 0,
  created_at    TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_branch_spend_branch
  ON branch_spend(branch, repository);

CREATE INDEX IF NOT EXISTS idx_branch_spend_cost
  ON branch_spend(repository, created_at DESC);
