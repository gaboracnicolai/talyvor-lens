-- 0132_prompt_embeddings_request_fp.sql — B15.1: a semantic cache row answers only the request it was stored for.
--
-- request_fp is cache.RequestFingerprint of the request that produced the row: everything that can
-- change the answer except the prompt text (system, tools, temperature, max_tokens, ...). Both
-- semantic lookups, private and pooled, require request_fp = the caller's fingerprint, so a similar
-- prompt can still be served but never one asked under different settings. Rows written before this
-- migration are NULL, and NULL = x is never true: they simply stop matching and age out.

ALTER TABLE prompt_embeddings ADD COLUMN IF NOT EXISTS request_fp TEXT;
