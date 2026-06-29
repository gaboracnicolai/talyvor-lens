-- 0070_routing_predictions.sql — Proof-of-Improvement piece 3 (proof-of-routing-prediction), PR-1:
-- the attributable routing-PREDICTION unit.
--
-- A prediction is a contributor's discrete, attributed assertion: "workspace W asserts — for cohort C,
-- route to model M." Recon (R1) found this unit does not exist: routing_patterns rows are anonymized
-- post-serve OBSERVATIONS averaged over ≥3 workspaces, not per-contributor predictions. This table is the
-- foundation that later PRs score (PR-3, on the held eval pool) and mint against (PR-4, via
-- HeldBenchmarkAnchor). PR-1 is an INERT DATA SUBSTRATE: no mint, no scoring, no change to the live
-- routing/serve/Advisor path.
--
-- COHORT C reuses the EXACT routing-intelligence dimensions (feature_category, input_token_range,
-- complexity_bucket — routing.go cohortKey/cohortKeyTiered) so PR-2 can tag eval items with the same keys
-- and PR-3 can resolve "cohort C's slice." MODEL M is a plain model-name string (provider optional in
-- PR-1; tightened in PR-3 when scoring resolves the endpoint).

CREATE TABLE IF NOT EXISTS routing_predictions (
    id                 TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    workspace_id       TEXT        NOT NULL,            -- the contributor asserting the prediction (attribution)
    feature_category   TEXT        NOT NULL,            -- cohort dim 1 (reuses routing_patterns taxonomy)
    input_token_range  TEXT        NOT NULL,            -- cohort dim 2
    complexity_bucket  TEXT        NOT NULL DEFAULT '', -- cohort dim 3 (tier); '' = untiered (routing's optional tier)
    model              TEXT        NOT NULL,            -- model M (plain name, as routing/catalog names it)
    provider           TEXT        NOT NULL DEFAULT '', -- provider of M (optional in PR-1)
    status             TEXT        NOT NULL DEFAULT 'pending', -- pending | active | retired (mirrors #250 lifecycle)
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Dedup (anti-hedge-farm): at most ONE LIVE prediction per (workspace, cohort). A workspace asserts
-- exactly one model per cohort; it cannot hedge many models to farm later. status='retired' frees the
-- slot so a new model can be asserted for that cohort. The partial predicate excludes retired rows.
CREATE UNIQUE INDEX IF NOT EXISTS idx_routing_predictions_live
    ON routing_predictions (workspace_id, feature_category, input_token_range, complexity_bucket)
    WHERE status IN ('pending', 'active');

-- Cohort lookup for the future scorer (PR-3): all live predictions for a cohort.
CREATE INDEX IF NOT EXISTS idx_routing_predictions_cohort
    ON routing_predictions (feature_category, input_token_range, complexity_bucket)
    WHERE status = 'active';
