-- 0135_prompt_embeddings_prompt_text.sql — B9.7: a pooled row keeps the question it answers.
--
-- The pair verifier (internal/pairverify, measured in docs/pool-b92-measured.md) judges a pooled
-- match by asking whether the STORED question and the ASKED one have the same answer, so it needs
-- both. Nicolai decided on 25 Sep 2026 that pooled rows may keep the contributor's question text.
--
-- Written only on pooled rows (SetPooled), exactly when the pooled response is. It has exactly the
-- protection the row's response already has: same table, same row,
-- deleted with it by whatever deletes the row, and no API selects it. A row with no text
-- (every row written before this migration) is never served while the verifier is wired.

ALTER TABLE prompt_embeddings ADD COLUMN IF NOT EXISTS prompt_text TEXT;
