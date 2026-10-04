-- B27.13: the private half of 0179. storedanswers.Counts, Delete and DeleteAllOf
-- find a workspace's private answers by workspace_id; idx_prompt_embeddings_private
-- (0053) leads with (provider, model), so it cannot serve that lookup and every
-- call seq-scanned the whole vector table.
--
-- Partial on NOT is_poolable, the same predicate those queries use. CONCURRENTLY
-- and alone in its file for the reasons 0179 gives.
-- lens:no-transaction

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_prompt_embeddings_private_workspace
  ON prompt_embeddings(workspace_id)
  WHERE NOT is_poolable;
