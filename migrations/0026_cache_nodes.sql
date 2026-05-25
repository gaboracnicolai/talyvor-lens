-- 0026_cache_nodes.sql — cache-node registry (Batch 3 Phase 3).
--
-- Sibling of inference_nodes (compute mining) — workspaces
-- volunteer cache capacity and earn LENS when their cached
-- entries serve cross-workspace requests.

CREATE TABLE IF NOT EXISTS cache_nodes (
    id                TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    workspace_id      TEXT NOT NULL,
    url               TEXT NOT NULL,
    max_size_gb       DOUBLE PRECISION NOT NULL DEFAULT 10,
    active            BOOLEAN NOT NULL DEFAULT TRUE,
    verified          BOOLEAN NOT NULL DEFAULT FALSE,
    node_secret_hash  TEXT,
    last_seen_at      TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_cache_nodes_workspace
    ON cache_nodes (workspace_id, active);

CREATE INDEX IF NOT EXISTS idx_cache_nodes_last_seen
    ON cache_nodes (last_seen_at);

CREATE TABLE IF NOT EXISTS cache_node_metrics (
    node_id      TEXT PRIMARY KEY REFERENCES cache_nodes(id) ON DELETE CASCADE,
    entries      INTEGER NOT NULL DEFAULT 0,
    size_mb      DOUBLE PRECISION NOT NULL DEFAULT 0,
    hit_rate     DOUBLE PRECISION NOT NULL DEFAULT 0,
    hits_total   BIGINT NOT NULL DEFAULT 0,
    last_updated TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
