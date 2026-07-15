-- 0094_k4_artifact_commitment.sql — H5 OPT-IN BUILDABLE-ARTIFACT COMMITMENT (the one missing seam).
--
-- WHY: today an output commits (k4_output_verdicts.response_sha256) to the RAW served response envelope — a
-- JSON blob, NOT a buildable tree. So the attestor's binding (sha256(supplied tree) == response_sha256)
-- refuses every real output, no talyvor_verified verdict is ever written, and H5 burns ONLY on a self-report
-- (a dishonest workspace never burns — fail-open).
--
-- THE SEAM: a workspace may OPT IN to commit, bound to the output, the sha256 of the exact BUILDABLE MODULE
-- it relies on (model output + minimal repo context). Both fields are NULLABLE — NULL = not opted in = today's
-- behavior EXACTLY (existing bonds/outputs unchanged, still bind response_sha256, still fail-safe). When set,
-- the attestor binds the supplied tree against artifact_sha256 instead of response_sha256, then reproduces the
-- build (compile-only) — everything downstream (IsSlashUsable, the appeal window, CAS, the four safety layers)
-- is untouched.
--
-- SOUNDNESS (generation-time binding, NOT bond-time supply): artifact_sha256 is a manifest hash over the
-- module's (path → content-sha256) entries, and the OUTPUT SLOT (artifact_output_path) is forced by the
-- committer to the output's ALREADY-COMMITTED response_sha256 — the actually-served bytes, locked at
-- generation. A workspace cannot bind a module whose output slot differs from what it served. Both fields are
-- append-once (set only while NULL, owner-bound); this migration only makes the storage layer safe for them.
ALTER TABLE k4_output_verdicts
    ADD COLUMN IF NOT EXISTS artifact_sha256      TEXT,   -- manifest hash of the committed buildable module; NULL = not opted in
    ADD COLUMN IF NOT EXISTS artifact_output_path TEXT;   -- the module path whose content is the served output (bound to response_sha256)

-- Defense in depth: an artifact commitment is all-or-nothing — either both fields are set or both are NULL.
-- A half-set row (a path with no hash, or a hash with no slot) can never exist.
ALTER TABLE k4_output_verdicts
    ADD CONSTRAINT k4_artifact_commitment_all_or_none
    CHECK ((artifact_sha256 IS NULL) = (artifact_output_path IS NULL));
