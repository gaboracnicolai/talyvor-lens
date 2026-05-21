CREATE TABLE IF NOT EXISTS ab_tests (
  id          TEXT PRIMARY KEY,
  provider    TEXT NOT NULL,
  model_a     TEXT NOT NULL,
  model_b     TEXT NOT NULL,
  sample_pct  FLOAT NOT NULL,
  min_samples INTEGER NOT NULL,
  active      BOOLEAN NOT NULL DEFAULT true,
  created_at  TIMESTAMPTZ DEFAULT NOW()
);
