-- 0041_workspace_cache_poolable.sql — Phase-2 Stage 2.0 shared-cache governance
-- gate (exact cache). Per-tenant opt-in for cross-user cache pooling.
--
--   cache_poolable — when TRUE, this workspace participates in the shared exact
--                    cache: its cacheable responses are ALSO written to an
--                    un-prefixed pooled key (tagged with this workspace as the
--                    contributor), and on a private-cache miss it may be served
--                    a pooled entry contributed by ANOTHER poolable workspace.
--                    A cross-tenant hit additionally requires the global switch
--                    (LENS_CACHE_POOLABLE_ENABLED) AND the contributor's own
--                    opt-in — pooling is impossible unless all three are true.
--
-- Defaults to FALSE: every existing and new workspace is private-by-default, so
-- the request path stays byte-for-byte unchanged until an admin opts a workspace
-- in (PUT /v1/workspaces/{wsID}/cache-poolable) and the operator flips the global
-- switch. Additive, idempotent (ADD COLUMN IF NOT EXISTS), no row rewrite — it
-- lands on the same workspaces table that already holds distill_policy (0039).

ALTER TABLE workspaces
  ADD COLUMN IF NOT EXISTS cache_poolable BOOLEAN NOT NULL DEFAULT false;
