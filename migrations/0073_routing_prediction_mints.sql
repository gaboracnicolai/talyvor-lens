-- 0073_routing_prediction_mints.sql — Proof-of-Improvement instance 2: proof-of-routing-prediction.
--
-- Rewards a contributor whose routing PREDICTION ("cohort C → model M") was PROVEN skill-above-baseline
-- on the verifier-held eval slice, paid on the measured skill_margin (NOT submission count) through the
-- SAME held-ledger / U6 chokepoint as every other mint. The score is produced upstream by the
-- routing-prediction SCORER (internal/routingscore), which already EXCLUDES the predictor's own
-- (and fingerprint-linked) authored eval items from the slice — so a self-dealt score can never exist.
-- This migration only adds the once-per-score mint claim; it touches no scorer/prediction table.
--
-- INERT: a mint requires LENS_ROUTING_PREDICTION_MINTING_ENABLED + LENS_PROOF_OF_IMPROVEMENT_ENABLED on
-- AND a positive LENS_ROUTING_PREDICTION_RATE_PER_POINT. Default rate 0 ⇒ the anchor refuses ⇒ no mint.

-- The once-per-scored-prediction mint claim — mirrors eval_contribution_mints / pool_royalty_mints EXACTLY
-- (the generic (request_id, contributor_workspace_id, minted_amount, status, finalize_after) finalize
-- columns + the idx_<table>_finalize partial index) so the generic FinalizeSweeper settles it UNCHANGED.
--   • request_id               IS the score's prediction_id (routing_prediction_scores.prediction_id, itself
--                              UNIQUE = score-once) → the once-per-prediction idempotency key.
--   • contributor_workspace_id IS the prediction's author (routing_predictions.workspace_id) — the payee.
--   • skill_margin             is the held-score paid on; minted_amount/status/finalize_after are read by
--                              the finalize kernel.
CREATE TABLE IF NOT EXISTS routing_prediction_mints (
    request_id               TEXT             PRIMARY KEY, -- = routing_prediction_scores.prediction_id (mint ONCE per scored prediction)
    contributor_workspace_id TEXT             NOT NULL,    -- the prediction's author (routing_predictions.workspace_id) — the payee
    skill_margin             DOUBLE PRECISION NOT NULL,    -- the held-score paid on: clamp01(m_avg − baseline_avg)
    minted_amount            DOUBLE PRECISION NOT NULL,    -- rate × skill_margin (what CreditHeldTx credited)
    status                   TEXT             NOT NULL DEFAULT 'held',
    finalize_after           TIMESTAMPTZ      NOT NULL,
    created_at               TIMESTAMPTZ      NOT NULL DEFAULT NOW()
);

-- The partial index the FinalizeSweeper SELECT rides (held rows past their finalize time).
CREATE INDEX IF NOT EXISTS idx_routing_prediction_mints_finalize
    ON routing_prediction_mints (finalize_after) WHERE status = 'held';
