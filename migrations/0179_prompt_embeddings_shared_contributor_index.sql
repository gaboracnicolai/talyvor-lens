-- B27.13: the stored-answers screen (storedanswers.Counts) and DELETE
-- /v1/workspaces/{ws}/stored-answers (storedanswers.Delete, DeleteAllOf) find a
-- workspace's shared answers by contributor_workspace_id. No index led with it,
-- so every call seq-scanned the whole vector table. 0180 is the private half.
--
-- Partial on is_poolable, the same predicate those queries use, so it stays
-- small and never indexes private rows.
--
-- CONCURRENTLY, so building it does not block writes to prompt_embeddings in
-- production. Postgres forbids that inside a transaction block — and runs a
-- multi-statement string as one — so this file is a single statement and the
-- private-rows index has its own file.
--
-- A failed concurrent build leaves an INVALID index that IF NOT EXISTS would
-- then skip: DROP INDEX CONCURRENTLY it and re-run migrate.
-- lens:no-transaction

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_prompt_embeddings_shared_contributor
  ON prompt_embeddings(contributor_workspace_id)
  WHERE is_poolable;
