-- Upgraded experiment system. Coexists with ab_tests (the
-- original shadow-A/B table) which keeps the proxy hot-path
-- shadow-probe contract working unchanged.
--
-- Variants + traffic_split live as jsonb so adding a Variant
-- field (system_prompt_override, weight, …) doesn't require a
-- schema migration. The trade-off: stats joins are awkward, but
-- we store all per-call aggregations in experiment_results
-- where the columns are stable.

CREATE TABLE IF NOT EXISTS experiments (
    id            TEXT PRIMARY KEY,
    workspace_id  TEXT NOT NULL,
    name          TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL,
    metric        TEXT NOT NULL,
    traffic_split JSONB NOT NULL,
    variants      JSONB NOT NULL,
    started_at    TIMESTAMPTZ,
    ended_at      TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_experiments_ws
    ON experiments(workspace_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_experiments_active
    ON experiments(workspace_id, status)
    WHERE status = 'running';

CREATE TABLE IF NOT EXISTS experiment_results (
    id            BIGSERIAL PRIMARY KEY,
    experiment_id TEXT NOT NULL REFERENCES experiments(id) ON DELETE CASCADE,
    variant_id    TEXT NOT NULL,
    user_id       TEXT NOT NULL DEFAULT '',
    latency_ms    BIGINT NOT NULL DEFAULT 0,
    quality_score DOUBLE PRECISION NOT NULL DEFAULT 0,
    cost_usd      DOUBLE PRECISION NOT NULL DEFAULT 0,
    tokens        INTEGER NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- The analysis query groups by (experiment_id, variant_id) and
-- the percentile uses latency_ms — this composite index lets
-- PG plan a single scan per analysis call.
CREATE INDEX IF NOT EXISTS idx_experiment_results_lookup
    ON experiment_results(experiment_id, variant_id);
