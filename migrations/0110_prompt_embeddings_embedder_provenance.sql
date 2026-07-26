-- 0110_prompt_embeddings_embedder_provenance.sql — record WHICH EMBEDDER produced each vector.
--
-- ⚠ THIS IS A CORRECTNESS FIX, NOT A PRIVACY ONE. prompt_embeddings.model is the COMPLETION
-- model (claude-sonnet, gpt-4o); nothing recorded the EMBEDDING model. Changing
-- LENS_EMBEDDING_MODEL therefore left vectors from two different models in one table, and the
-- similarity query compared them to each other. Embeddings from different models occupy
-- different vector spaces: the cosine between them is meaningless, not merely shifted, and it
-- can land above the threshold by coincidence — at which point the cache serves the OLD entry's
-- response to an UNRELATED new query. A wrong answer, returned as a hit, with no error.
--
-- This bites a SINGLE-TENANT SELF-HOSTER WITH POOLING OFF. It needs no second workspace and no
-- cross-tenant sharing, only an operator changing the embedding model for cost or latency.
--
-- EXISTING ROWS ARE LEFT NULL, DELIBERATELY.
--
--   Backfilling with the current default would assert a fact nobody recorded: if the model was
--   ever changed, those rows came from a different one, and stamping today's name on them
--   manufactures exactly the provenance whose absence caused the bug. It would also be the most
--   dangerous option, because it makes incomparable vectors look comparable.
--
--   Deleting them is safe but discards a working cache irreversibly, and is unnecessary: the
--   read path treats NULL as not-comparable, so an unknown-provenance row is already inert, and
--   the existing retention sweeper (semanticDeleteStaleSQL) reclaims it on the normal schedule.
--
--   NULL is the honest record: we do not know, and the read rule says so. The cost is that the
--   semantic cache goes cold once on deploy and refills from live traffic. That is the price of
--   not having recorded provenance until now, and it is paid once.
--
-- An operator who KNOWS the model never changed may adopt the existing rows deliberately:
--   UPDATE prompt_embeddings SET embedding_model = '<the model that produced them>'
--    WHERE embedding_model IS NULL;
-- That is a claim about history only they can make, which is why it is not done here.
--
-- Additive and idempotent: ADD COLUMN IF NOT EXISTS, nullable, no default, no row rewrite.

ALTER TABLE prompt_embeddings
  ADD COLUMN IF NOT EXISTS embedding_model TEXT;

-- The similarity searches filter on embedding_model alongside provider/model, so it belongs in
-- the same index shape they already use. Partial: NULL rows are never served, so indexing them
-- would only cost writes.
CREATE INDEX IF NOT EXISTS idx_prompt_embeddings_embedder
  ON prompt_embeddings(provider, model, embedding_model)
  WHERE embedding_model IS NOT NULL;

-- ── The boot binding ──────────────────────────────────────────────────────────────────
--
-- pool_safety_attestation records the (embedding model, threshold) that last PASSED
-- `lens poolcheck`. The gateway compares the live configuration against it at boot.
--
-- Why this table exists: the preflight proved a configuration safe at deploy time, but its
-- result was ephemeral — nothing carried it forward, so a later change to
-- LENS_EMBEDDING_MODEL or LENS_SEMANTIC_THRESHOLD silently invalidated a verdict nobody
-- could see. A control that only fires on the careful path is not a control on the careless
-- one, and the careless path (editing .env and restarting) is the one that needs it.
--
-- Single row by construction: this is a property of the deployment, not a history.
CREATE TABLE IF NOT EXISTS pool_safety_attestation (
  id              BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),
  embedding_model TEXT        NOT NULL,
  threshold       DOUBLE PRECISION NOT NULL,
  worst_pair      TEXT        NOT NULL DEFAULT '',
  worst_score     DOUBLE PRECISION NOT NULL DEFAULT 0,
  checked_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
