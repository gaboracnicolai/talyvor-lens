-- doc2query variants: extra match targets pointing at ONE stored answer.
--
-- A variant is a question derived from a stored ANSWER at write time and embedded as an additional
-- vector. It exists because #392's entity gate made pooling safe but left it nearly useless — the
-- safe window (0.807–0.849) served 1 of 28 measured rephrasings. Two people asking the same thing
-- often are not similar to EACH OTHER, but both are similar to the answer.
--
-- variant_of points at the original row. It is NULL for originals.
--
-- ⚠ IT IS NOT DECORATION — IT DECIDES WHO GETS PAID AND WHETHER ANTI-GAMING WORKS. The pooled read
-- returns COALESCE(variant_of, id) as the entry id. poolroyalty/detector.go aggregates claims with
-- GROUP BY entry_id for ring detection and per-entry rate limiting; if each variant reported its
-- own id, one answer would present as N independent entries and every count would be divided by N,
-- letting an attacker stay under thresholds by spreading hits across variants of one answer.
--
-- ⚠ VARIANT ROWS COPY THE ORIGINAL'S discriminators, NEVER THEIR OWN TEXT'S. A model deriving
-- questions from a Pydantic v2 answer will produce version-less phrasings; carrying their own
-- entities would make them unconstrained match targets for version-specific content, re-opening
-- the hole 0112 closed. Enforced at the write site in internal/cache.
--
-- ON DELETE CASCADE: an original's variants are meaningless without it, and the retention sweeper
-- deletes by age. Leaving orphans would strand match targets pointing at a response that is gone.
ALTER TABLE prompt_embeddings
  ADD COLUMN IF NOT EXISTS variant_of UUID REFERENCES prompt_embeddings(id) ON DELETE CASCADE;

CREATE INDEX IF NOT EXISTS idx_prompt_embeddings_variant_of
  ON prompt_embeddings (variant_of)
  WHERE variant_of IS NOT NULL;
