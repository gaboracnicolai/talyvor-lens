-- 0021_embedding_nodes.sql — embedding-mining registry (Batch 2 Item 3).
--
-- Sibling of inference_nodes (0020). Same shape — workspace_id +
-- url + provider-ish info + verified flag — but the workload
-- (sentence embeddings) is CPU-friendly so a separate table
-- keeps the GIN(models) index focused on multi-model lookups
-- per workload.

CREATE TABLE IF NOT EXISTS embedding_nodes (
    id            TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    workspace_id  TEXT NOT NULL,
    url           TEXT NOT NULL,
    model         TEXT NOT NULL,
    dimensions    INTEGER NOT NULL DEFAULT 1536,
    max_batch     INTEGER NOT NULL DEFAULT 100,
    speed_tps     INTEGER NOT NULL DEFAULT 500,
    active        BOOLEAN NOT NULL DEFAULT TRUE,
    verified      BOOLEAN NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_embedding_nodes_workspace
    ON embedding_nodes (workspace_id, active);

CREATE INDEX IF NOT EXISTS idx_embedding_nodes_model
    ON embedding_nodes (model, verified, active);
