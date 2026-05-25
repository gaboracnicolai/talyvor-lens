-- 0025_node_heartbeat.sql — node-secret hashing + heartbeat
-- timestamps for the inference network (Batch 3 Phase 2).
--
-- Additive ALTERs only — existing nodes keep working; the new
-- columns default to NULL / 0 so the heartbeat endpoint and the
-- secret-validation path degrade gracefully against rows from
-- before this migration.

ALTER TABLE inference_nodes
    ADD COLUMN IF NOT EXISTS node_secret_hash TEXT,
    ADD COLUMN IF NOT EXISTS last_seen_at     TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS uptime_seconds   BIGINT NOT NULL DEFAULT 0;

-- Lens uses (last_seen_at < NOW() - 90s) → unhealthy. Index it
-- so the periodic sweep + the "nodes_active" rollup stay fast.
CREATE INDEX IF NOT EXISTS idx_inference_nodes_last_seen
    ON inference_nodes (last_seen_at);
