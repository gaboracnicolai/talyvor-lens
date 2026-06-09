-- 0049_pattern_mine_credits.sql — idempotency claim for routing-pattern EARNING (S3).
--
-- One credit per (request_id, workspace_id): a COMPOSITE UNIQUE, DELIBERATELY
-- NOT the bare UNIQUE(request_id) of pool_royalty_mints. request_id is the
-- caller-supplied X-Talyvor-Request-ID; a bare global UNIQUE would let workspace
-- A suppress workspace B's legitimate earn by colliding the header (B's ON
-- CONFLICT DO NOTHING returns 0 → B silently earns nothing). Scoping uniqueness
-- per-workspace blocks that griefing seam while still deduping a retry/replay
-- WITHIN a workspace. (Pool-B is safe with bare request_id because its row pairs
-- requester/contributor identity; this is a bare per-request stamp on a
-- SELF-generated earn, so the workspace must be IN the key.)
--
-- UNPARTITIONED, like pool_royalty_mints / povi_receipts / povi_challenges: a
-- UNIQUE is illegal on the hash-partitioned hot tables (their composite PK must
-- include the partition key). Additive only — no change to existing tables, no
-- advisory locks (within the migration-audit invariants).
--
-- No pattern_id: the claim is written FIRST (before the routing_patterns row
-- exists), so it is a pure per-(request, workspace) dedup stamp; the
-- routing_patterns row remains the attribution record. `earned` rides for audit.
CREATE TABLE IF NOT EXISTS pattern_mine_credits (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id   TEXT NOT NULL,
    workspace_id TEXT NOT NULL,
    earned       DOUBLE PRECISION NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (request_id, workspace_id)
);
