CREATE TABLE IF NOT EXISTS eval_test_cases (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    workspace_id    TEXT NOT NULL DEFAULT 'default',
    provider        TEXT NOT NULL,
    model           TEXT NOT NULL,
    prompt          TEXT NOT NULL,
    expected_output TEXT NOT NULL DEFAULT '',
    eval_method     TEXT NOT NULL DEFAULT 'heuristic',
    pass_threshold  FLOAT NOT NULL DEFAULT 0.6,
    tags            TEXT[] NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS eval_results (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    test_case_id TEXT NOT NULL,
    run_id       TEXT NOT NULL,
    passed       BOOLEAN NOT NULL,
    score        FLOAT NOT NULL,
    latency_ms   INTEGER NOT NULL DEFAULT 0,
    cost_usd     FLOAT NOT NULL DEFAULT 0,
    eval_method  TEXT NOT NULL,
    error        TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS eval_runs (
    id             TEXT PRIMARY KEY,
    workspace_id   TEXT NOT NULL DEFAULT 'default',
    total_tests    INTEGER NOT NULL DEFAULT 0,
    passed         INTEGER NOT NULL DEFAULT 0,
    failed         INTEGER NOT NULL DEFAULT 0,
    pass_rate      FLOAT NOT NULL DEFAULT 0,
    total_cost_usd FLOAT NOT NULL DEFAULT 0,
    avg_latency_ms INTEGER NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ DEFAULT NOW(),
    completed_at   TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_eval_results_run
    ON eval_results(run_id);

CREATE INDEX IF NOT EXISTS idx_eval_runs_workspace
    ON eval_runs(workspace_id, created_at DESC);
