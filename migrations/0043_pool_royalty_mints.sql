-- 0043_pool_royalty_mints.sql — Phase-2 Stage 2.1: the Pool-B royalty mint
-- claim table (COORDINATION Stages 2.1–2.5; seam #1).
--
-- A served cross-tenant pooled cache hit mints s × avoided_COGS to the
-- contributing tenant — EXACTLY ONCE per serving request. This table is the
-- idempotency claim: the mint flow INSERTs a claim row ON CONFLICT
-- (request_id) DO NOTHING and proceeds to the ledger credit ONLY when the
-- insert claimed the row (RowsAffected == 1), in the SAME transaction. The
-- same claim-then-act shape as povi_challenges' double-slash guard (0033).
--
-- request_id ALONE is the unique key — deliberately. The key must derive only
-- from the logical serving request, never from what the cache lookup found:
-- a retried request can legitimately re-match a DIFFERENT semantic entry
-- (ORDER BY similarity LIMIT 1 over a moving 24h window), so keying on the
-- match (entry id / contributor) would reintroduce the double-mint this
-- table exists to prevent. Entry identity and contributor are recorded as
-- attribution DATA below, not as part of the key. A colliding request_id can
-- only SUPPRESS a later mint (deflationary, safe) — never inflate supply.
--
-- UNPARTITIONED, like povi_receipts / povi_challenges: a bare UNIQUE
-- (request_id) is illegal on the hash-partitioned hot tables (their composite
-- PK must include workspace_id — and a cross-tenant hit has TWO workspaces,
-- so neither choice would be right). Volume is one row per pooled serve,
-- far below the hot-write tables 0034 partitioned.

CREATE TABLE IF NOT EXISTS pool_royalty_mints (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id               TEXT NOT NULL UNIQUE,             -- one mint per serving request (the idempotency key)
    requester_workspace_id   TEXT NOT NULL,
    contributor_workspace_id TEXT NOT NULL,
    layer                    TEXT NOT NULL,                    -- exact | semantic
    entry_id                 TEXT NOT NULL DEFAULT '',         -- exact: pooled cache key; semantic: prompt_embeddings.id
    provider                 TEXT NOT NULL DEFAULT '',
    model                    TEXT NOT NULL DEFAULT '',
    similarity               DOUBLE PRECISION NOT NULL DEFAULT 0,
    avoided_cogs_usd         DOUBLE PRECISION NOT NULL DEFAULT 0,
    minted_amount            DOUBLE PRECISION NOT NULL DEFAULT 0,  -- s × avoided_COGS actually credited
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_pool_royalty_mints_contributor
    ON pool_royalty_mints (contributor_workspace_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_pool_royalty_mints_requester
    ON pool_royalty_mints (requester_workspace_id, created_at DESC);
