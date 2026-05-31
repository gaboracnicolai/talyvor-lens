-- 0035_controlplane.sql — embedding-node heartbeat parity.
--
-- inference_nodes gained last_seen_at / uptime_seconds / node_secret_hash in
-- 0025; cache_nodes got last_seen_at / node_secret_hash in 0026.
-- embedding_nodes was left behind.  This migration brings it to parity so the
-- control-plane reconciler can treat all three node types uniformly when
-- sweeping for stale nodes.

ALTER TABLE embedding_nodes
    ADD COLUMN IF NOT EXISTS node_secret_hash TEXT,
    ADD COLUMN IF NOT EXISTS last_seen_at     TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS uptime_seconds   BIGINT NOT NULL DEFAULT 0;

-- The reconciler sweep query is: last_seen_at < NOW() - 90s AND active = TRUE.
-- Index on last_seen_at keeps that sweep fast even as the table grows.
CREATE INDEX IF NOT EXISTS idx_embedding_nodes_last_seen
    ON embedding_nodes (last_seen_at);
