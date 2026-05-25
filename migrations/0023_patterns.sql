-- 0023_patterns.sql — pattern-intelligence mining (Batch 2 Item 5).
--
-- routing_patterns stores anonymised per-request routing
-- observations (feature + model + provider + bucketed input
-- size + bucketed latency + quality + cache-hit-rate) together
-- with the rarity score computed at insert time and an opted_in
-- flag that gates whether the row contributes to the collective
-- insights endpoint.
--
-- workspace_pattern_optin holds the per-workspace toggle —
-- separate row so the proxy hot path can do a single PK lookup
-- to decide whether to record patterns at all.

CREATE TABLE IF NOT EXISTS routing_patterns (
    id                TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    workspace_id      TEXT NOT NULL,
    feature_category  TEXT NOT NULL,
    model_used        TEXT NOT NULL,
    provider_used     TEXT NOT NULL,
    input_token_range TEXT NOT NULL,
    output_quality    DOUBLE PRECISION NOT NULL DEFAULT 0,
    latency_bucket    TEXT NOT NULL,
    cache_hit_rate    DOUBLE PRECISION NOT NULL DEFAULT 0,
    success_rate      DOUBLE PRECISION NOT NULL DEFAULT 1,
    sample_count      INTEGER NOT NULL DEFAULT 1,
    rarity            DOUBLE PRECISION NOT NULL DEFAULT 0,
    opted_in          BOOLEAN NOT NULL DEFAULT FALSE,
    earned            DOUBLE PRECISION NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_patterns_workspace
    ON routing_patterns (workspace_id, created_at DESC);

-- Partial index keyed on the composite "what pattern is this"
-- key only when opted_in. The GetInsights aggregation and
-- ScoreRarity both filter on opted_in = true, so we never pay
-- for the index on private rows.
CREATE INDEX IF NOT EXISTS idx_patterns_lookup
    ON routing_patterns (feature_category, model_used, provider_used,
                         input_token_range, latency_bucket)
    WHERE opted_in = TRUE;

CREATE TABLE IF NOT EXISTS workspace_pattern_optin (
    workspace_id TEXT PRIMARY KEY,
    opted_in_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
