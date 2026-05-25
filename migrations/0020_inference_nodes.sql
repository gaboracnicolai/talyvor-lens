-- 0020_inference_nodes.sql — compute-mining registry (Batch 2 Item 2).
--
-- inference_nodes records a workspace's volunteered GPU endpoint.
-- node_metrics stores rolling stats (separate row so the
-- per-request counter UPDATEs don't fight for the inference_nodes
-- row lock).

CREATE TABLE IF NOT EXISTS inference_nodes (
    id              TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    workspace_id    TEXT NOT NULL,
    url             TEXT NOT NULL,
    provider        TEXT NOT NULL,
    models          TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[],
    gpu_type        TEXT NOT NULL DEFAULT 'cpu',
    max_concurrent  INTEGER NOT NULL DEFAULT 1,
    price_per_token DOUBLE PRECISION NOT NULL DEFAULT 0.050,
    active          BOOLEAN NOT NULL DEFAULT TRUE,
    verified        BOOLEAN NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_nodes_workspace
    ON inference_nodes (workspace_id, active);

CREATE INDEX IF NOT EXISTS idx_nodes_models
    ON inference_nodes USING GIN (models);

CREATE TABLE IF NOT EXISTS node_metrics (
    node_id          TEXT PRIMARY KEY REFERENCES inference_nodes(id) ON DELETE CASCADE,
    requests_served  INTEGER NOT NULL DEFAULT 0,
    tokens_served    BIGINT NOT NULL DEFAULT 0,
    avg_latency_ms   BIGINT NOT NULL DEFAULT 0,
    error_rate       DOUBLE PRECISION NOT NULL DEFAULT 0,
    uptime_pct       DOUBLE PRECISION NOT NULL DEFAULT 100,
    last_active_at   TIMESTAMPTZ
);
