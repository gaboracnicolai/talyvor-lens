-- Entity discriminators for the shared pool.
--
-- ⚠ THE DEFECT THIS CLOSES, measured on engineering traffic with text-embedding-3-small:
-- "How do I write a validator in Pydantic v1?" and "…v2?" score 0.9579 — above the production
-- threshold of 0.92 — so the pool serves one answer for both questions, cross-tenant. Vue 2/3
-- (0.9450), Tailwind v3/v4 (0.9265) and React Router v5/v6 (0.9207) do the same. The best GENUINE
-- rephrasing measured 0.8488, so the two populations are INVERTED by 0.109 and no threshold
-- separates them. Similarity cannot be tuned into a fix; the entity check has to be separate.
--
-- discriminators holds a sorted, canonical rendering of the entities named by the prompt
-- (versions, error codes, identifiers, technologies, scoped commands). The pooled read requires an
-- EXACT match on it in addition to the vector distance.
--
-- ⚠ NULLABLE, AND NULL IS LOAD-BEARING. prompt_embeddings stores prompt_hash, never the prompt
-- text, so rows written before this migration cannot have their discriminators derived — the
-- source text is gone. `discriminators = $n` is NULL for those rows, which is not TRUE, so they
-- stop being served by the pooled path. That is the intended fail-closed behaviour: an
-- unverifiable row is refused rather than trusted. Those rows remain readable by the PRIVATE path
-- (unchanged) and age out via the existing retention window.
--
-- Additive and idempotent: ADD COLUMN IF NOT EXISTS, nullable, no default, no row rewrite.
ALTER TABLE prompt_embeddings
  ADD COLUMN IF NOT EXISTS discriminators TEXT;

-- The pooled read filters on (provider, model, embedding_model, is_poolable, discriminators)
-- before ordering by vector distance. Indexing the equality key keeps that filter cheap as the
-- pool grows.
CREATE INDEX IF NOT EXISTS idx_prompt_embeddings_pooled_discriminators
  ON prompt_embeddings (provider, model, discriminators)
  WHERE is_poolable = true;
