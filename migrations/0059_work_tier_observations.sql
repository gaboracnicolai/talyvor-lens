-- 0059_work_tier_observations.sql
-- WorkTier — DESCRIPTIVE work classification (Master Plan WorkTier). A per-served-
-- request record of the DERIVED tier (the locked API contract: size / cost /
-- complexity / sensitivity) PLUS the RAW signal behind EVERY axis, so historical
-- rows are re-bucketable offline as thresholds / the complexity scorer evolve —
-- freeze the interface, not the implementation.
--
-- DESCRIPTIVE, NEVER INCENTIVIZED: nothing mints from a tier (the descriptive-
-- never-incentivized doctrine). Observed post-serve, consumed (LATER) only by the
-- routing Advisor + analytics. Default-off (LENS_WORKTIER_ENABLED), per-workspace
-- scoped. NON-CONTENT columns ONLY — no prompt, no response, no matched PII span,
-- no analysis internals; just magnitudes, the derived buckets, and boolean causes.
CREATE TABLE IF NOT EXISTS work_tier_observations (
    id           BIGSERIAL PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    feature      TEXT NOT NULL DEFAULT '',
    model        TEXT NOT NULL DEFAULT '',
    provider     TEXT NOT NULL DEFAULT '',
    -- the 4 DERIVED buckets (the frozen contract consumers switch on)
    size_bucket  TEXT NOT NULL,
    cost_bucket  TEXT NOT NULL,
    complexity   TEXT NOT NULL,
    sensitivity  TEXT NOT NULL,
    -- the RAW signal behind each axis (re-bucketable implementation):
    input_tokens     INTEGER NOT NULL DEFAULT 0,          -- size: split kept (in/out shapes differ)
    output_tokens    INTEGER NOT NULL DEFAULT 0,
    cost_usd         DOUBLE PRECISION NOT NULL DEFAULT 0,  -- cost
    complexity_score INTEGER NOT NULL DEFAULT 0,           -- complexity: raw [0,5] from router.AnalyseComplexity
    pii_detected     BOOLEAN NOT NULL DEFAULT FALSE,       -- sensitivity cause A (distinct from B)
    guardrail_fired  BOOLEAN NOT NULL DEFAULT FALSE,       -- sensitivity cause B (a safety guardrail tripped)
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- The read-side per-workspace volume-profile aggregate (sliceable by model — the
-- Advisor's quality-per-dollar-per-model need) scans (workspace_id, created_at).
CREATE INDEX IF NOT EXISTS idx_work_tier_ws ON work_tier_observations (workspace_id, created_at DESC);
