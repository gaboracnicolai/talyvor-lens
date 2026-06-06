-- 0042_prompt_embeddings_pooling.sql — Phase-2 Stage 2.0b: extend the
-- shared-cache governance gate to the SEMANTIC cache (prompt_embeddings).
--
--   contributor_workspace_id — the workspace that contributed a POOLED row
--                              (the cross-tenant-readable copy). NULL on private
--                              rows and on every pre-0042 row.
--   is_poolable              — TRUE only on rows written to the shared pool by an
--                              opted-in contributor. The private similarity
--                              search filters these OUT (is_poolable = false) and
--                              the pooled search filters these IN, so private and
--                              pooled rows never mix. A cross-tenant pooled hit
--                              additionally requires the global switch + the
--                              requester's opt-in + the contributor's live opt-in
--                              (verified against contributor_workspace_id).
--
-- Both columns default to the PRIVATE case (is_poolable false, contributor NULL),
-- so every existing row is non-poolable and the request path stays byte-for-byte
-- unchanged until a workspace opts in and the global switch is on. Additive,
-- idempotent (ADD COLUMN IF NOT EXISTS), no row rewrite. Mirrors the exact-cache
-- column added in 0041.

ALTER TABLE prompt_embeddings
  ADD COLUMN IF NOT EXISTS contributor_workspace_id TEXT;

ALTER TABLE prompt_embeddings
  ADD COLUMN IF NOT EXISTS is_poolable BOOLEAN NOT NULL DEFAULT false;

-- Pooled rows are a small fraction of the table; a partial index keeps the
-- cross-tenant similarity search scoped without bloating the common (private)
-- path's index footprint.
CREATE INDEX IF NOT EXISTS idx_prompt_embeddings_poolable
  ON prompt_embeddings(provider, model)
  WHERE is_poolable;
