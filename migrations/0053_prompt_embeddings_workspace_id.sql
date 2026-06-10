-- 0053_prompt_embeddings_workspace_id.sql — #142: hard workspace boundary on
-- the PRIVATE semantic cache path.
--
-- The private semantic lookup (internal/cache/semantic.go semanticSelectSQL)
-- had NO workspace filter — it ranged over every workspace's is_poolable=false
-- rows and returned the single nearest embedding. Tenant isolation rested
-- PURELY on the wsID: prefix shifting the embedding past SemanticThreshold (0.92
-- cosine), which is a length-dependently soft boundary: for a long prompt the
-- short prefix barely moves the embedding, so two tenants' near-identical long
-- prompts can collide above threshold (a cross-tenant cache read). This adds a
-- HARD boundary — the embedding stays the similarity RANKER, workspace_id
-- becomes an exact pre-rank FILTER, matching the exact cache's hash isolation.
--
-- NULLABLE on purpose: pre-existing private rows get workspace_id=NULL, which
-- matches NO caller's `workspace_id = $N` filter, so they become cache-cold and
-- self-heal (re-populated on the next miss). A cache, not data — acceptable.
--
-- The POOLED path (is_poolable=true, consent-gated cross-tenant sharing) is
-- DELIBERATELY unaffected: its lookup keeps ranging is_poolable=true and gains
-- no workspace_id filter. workspace_id is the private rows' owner; pooled rows
-- already carry contributor_workspace_id for their (separate) attribution.
--
-- Additive + idempotent: ADD COLUMN IF NOT EXISTS / CREATE INDEX IF NOT EXISTS,
-- no row rewrite. Mirrors 0042's pooling columns.

ALTER TABLE prompt_embeddings
  ADD COLUMN IF NOT EXISTS workspace_id TEXT;

-- Complements (does not overlap) idx_prompt_embeddings_poolable
-- (provider, model) WHERE is_poolable: this is the disjoint private-rows
-- partition, with workspace_id added to support the new equality filter. Partial
-- on is_poolable=false so it stays small and never indexes pooled rows.
CREATE INDEX IF NOT EXISTS idx_prompt_embeddings_private
  ON prompt_embeddings(provider, model, workspace_id)
  WHERE is_poolable = false;
