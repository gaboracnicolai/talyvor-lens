-- 0098_output_content_binding.sql — H5 CONTENT BINDING: the hash a buildable tree CAN carry.
--
-- WHY: response_sha256 (0084) hashes the RAW served envelope — a JSON blob. It is IDENTITY (an input to
-- output_id) and stays byte-frozen forever. But the 0094 artifact commitment folded THAT hash into the
-- output slot, so a matching tree had to contain the raw envelope at the slot path — never buildable
-- source, and the agent discards the envelope bytes anyway. Result: internal/attest could never attest a
-- real output; H5 stayed fail-open in practice.
--
-- THE FIX: a SECOND, content-level hash. output_content_sha256 = hex(sha256(canonical assistant text)) —
-- the exact bytes the flagship writer materializes on disk (outputverify.CanonicalContent pins the byte
-- definition: provider-shaped extraction, fence strip, outer trim, exactly-one trailing newline). Computed
-- POST-FLUSH at capture alongside response_sha256; NULL when the served body has no committable content
-- (true SSE streams are never captured; extraction failure; empty text) — behavior unchanged for those.
-- The 0094 committer now folds THIS hash into the output slot, so the committed manifest is satisfiable by
-- a real tree whose slot file IS the generated code — buildable, attestable, end-to-end.
--
-- IDENTITY UNTOUCHED: response_sha256 and DeriveOutputID are byte-unchanged (pinned-value proof in
-- internal/proxy). This migration only ADDS a nullable column; NULL = today's behavior exactly.
--
-- ZERO-DATA NOTE: no artifact_sha256 row exists anywhere (LENS_H5_ARTIFACT_ENABLED was never set; the
-- gated endpoint is the only writer), so re-pointing the slot semantics from envelope-hash to content-hash
-- carries no data-migration burden and can never orphan an old commitment.
ALTER TABLE k4_output_verdicts
    ADD COLUMN IF NOT EXISTS output_content_sha256 TEXT;  -- hex(sha256(canonical content)); NULL = not committable

-- Defense in depth: an artifact commitment REQUIRES a content binding (the committer refuses without one);
-- a row claiming an artifact while lacking the content hash it folds can never exist. Safe on existing
-- data: every existing row has artifact_sha256 NULL (zero-data note above).
ALTER TABLE k4_output_verdicts
    ADD CONSTRAINT k4_artifact_requires_content
    CHECK (artifact_sha256 IS NULL OR output_content_sha256 IS NOT NULL);
