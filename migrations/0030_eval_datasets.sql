-- 0030_eval_datasets.sql — golden-dataset evaluation + scheduling (Master Plan
-- Upgrade 17).
--
-- Extends the existing eval schema (0012) with:
--   eval_datasets   — a named, per-workspace collection of test cases (the
--                     golden set a model/prompt change is regression-tested
--                     against).
--   dataset_id      — links eval_test_cases and eval_runs to a dataset, so a
--                     run's per-case results can be diffed against the prior
--                     run on the SAME dataset to surface regressions.
--   eval_schedules  — a dataset + cadence the background scheduler runs
--                     automatically, so quality regressions are caught without
--                     a manual trigger. Off the request hot path entirely.

CREATE TABLE IF NOT EXISTS eval_datasets (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL DEFAULT 'default',
    name         TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ DEFAULT NOW()
);

-- Cases and runs gain an optional dataset linkage. NULL = a legacy,
-- workspace-level case/run not associated with any dataset (AddTestCase path).
ALTER TABLE eval_test_cases ADD COLUMN IF NOT EXISTS dataset_id TEXT;
ALTER TABLE eval_runs       ADD COLUMN IF NOT EXISTS dataset_id TEXT;

CREATE TABLE IF NOT EXISTS eval_schedules (
    id               TEXT PRIMARY KEY,
    workspace_id     TEXT NOT NULL DEFAULT 'default',
    dataset_id       TEXT NOT NULL,
    interval_seconds INTEGER NOT NULL,
    enabled          BOOLEAN NOT NULL DEFAULT TRUE,
    target_model     TEXT NOT NULL DEFAULT '',
    last_run_at      TIMESTAMPTZ,
    created_at       TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_eval_datasets_workspace
    ON eval_datasets(workspace_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_eval_test_cases_dataset
    ON eval_test_cases(dataset_id);

CREATE INDEX IF NOT EXISTS idx_eval_runs_dataset
    ON eval_runs(dataset_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_eval_schedules_enabled
    ON eval_schedules(enabled);
