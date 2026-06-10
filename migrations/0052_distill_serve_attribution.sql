-- 0052_distill_serve_attribution.sql — S1: distill attribution-WITHOUT-mint.
--
-- A durable, MINT-FREE per-(owner, requester, artifact) counter of CONSENTED
-- cross-tenant pooled-distill serves: who contributed a distill artifact, which
-- workspace it was served to, and how often. Descriptive and inert — there is
-- NO money attached, NO ledger, NO caps/holdback/status, NO request_id claim
-- (WorkTier descriptive-not-incentivized; see ROADMAP.md "DISTILL economy").
--
-- Feeds the PARKED distill-reuse-royalty condition-(b) decision ("is there
-- material cross-tenant reuse in prod?"): serve_count answers that directly. The
-- counter idiom mirrors the cache layer's hit_count = hit_count + 1 UPSERT
-- (internal/cache/semantic.go), not a per-event row, so re-serving the same
-- artifact bumps the count rather than appending — exactly the materiality
-- signal, with bounded retention.
--
-- WRITE-ONLY for now: this release writes these rows but exposes NO read
-- endpoint. The read surface (admin-only raw rows; counterparty identity masked
-- to a count; content_hash never returned cross-tenant) is its own later step.
--
-- A row is written ONLY when the three-switch distill-pooling consent already
-- authorized the cross-tenant serve (LENS_DISTILL_POOLABLE_ENABLED + owner
-- opt-in + requester opt-in, #141), AND owner != requester (self-serve skipped),
-- AND the requester's logging policy is not None. The composite PK is the tuple
-- itself, so the table is unpartitioned (like pool_royalty_mints / pattern_mine_credits).

CREATE TABLE IF NOT EXISTS distill_serve_attribution (
    owner_workspace_id     TEXT        NOT NULL,   -- contributor (the pooled artifact's GetWithOwner owner)
    requester_workspace_id TEXT        NOT NULL,   -- who was served (the requesting workspace)
    content_hash           TEXT        NOT NULL,   -- distill.ContentHash(raw document bytes)
    serve_count            BIGINT      NOT NULL DEFAULT 0,
    first_served_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_served_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (owner_workspace_id, requester_workspace_id, content_hash)
);

-- Hot read path for the future materiality probe: "how much was each owner's
-- artifact reused cross-tenant, recently".
CREATE INDEX IF NOT EXISTS idx_distill_serve_attribution_owner
    ON distill_serve_attribution (owner_workspace_id, last_served_at DESC);
