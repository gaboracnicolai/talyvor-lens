-- 0106_workspace_cache_poolable_default_true.sql — NEW workspaces default to
-- cross-tenant cache pooling (cache_poolable).
--
-- This flips ONLY the column DEFAULT. ALTER COLUMN ... SET DEFAULT is a
-- catalog-only change (it rewrites pg_attrdef, not the table): it performs NO
-- UPDATE and touches NO existing row. Every workspace already in the table keeps
-- its stored cache_poolable byte-for-byte — a workspace that never consented to
-- cross-tenant sharing (false) STAYS false. Only rows inserted from here on pick
-- up the new default.
--
-- Why not a data backfill: cache_poolable is SYMMETRIC — one column gates both
-- BENEFITING from and CONTRIBUTING to the shared cache — so flipping an existing
-- row to true would begin exposing that tenant's response content cross-tenant.
-- That consent cannot be granted retroactively, so existing rows are deliberately
-- left untouched; a new default is only ever applied at creation time.
--
-- 0041 created the column at DEFAULT false; this migration changes only that
-- default. The application (workspace.RegisterWorkspace) mirrors it: a new
-- workspace is registered poolable=true, an existing one is preserved.

ALTER TABLE workspaces
  ALTER COLUMN cache_poolable SET DEFAULT true;
