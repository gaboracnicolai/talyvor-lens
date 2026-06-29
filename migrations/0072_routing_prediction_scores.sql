-- 0072_routing_prediction_scores.sql — Proof-of-Improvement piece 3 (proof-of-routing-prediction), PR-3a:
-- the SKILL-ABOVE-BASELINE score of a routing prediction.
--
-- A prediction "cohort C → model M" (PR-1) is PROVEN by running M and the baseline default route on
-- cohort C's held eval slice (PR-2's cohort tags), scoring each vs verifier-held expected_output
-- (eval.StaticScore), and recording M's correctness MARGIN over the baseline:
--   skill_margin = clamp01(avg_score(M on slice) − avg_score(baseline on slice))
-- 0 when M does not beat the baseline — so "route to the obvious best model = the baseline" pays ~0; only
-- a genuine outperformer scores. PR-4 mints on skill_margin via HeldBenchmarkAnchor.
--
-- PR-3a builds the FULL scorer behind an Inferer interface with a FAKE implementation only — NO real
-- inference, NO mint, NO serve-path touch. The score is INERT data until PR-3b supplies a real Inferer.
--
-- SCORE-ONCE: prediction_id is UNIQUE, so an active prediction is scored at most once; retiring +
-- resubmitting (PR-1) creates a NEW prediction id → a fresh score. A prediction whose cohort slice is
-- below the min-slice floor, or whose baseline is unresolvable (Advisor BasisNone), produces NO row.

CREATE TABLE IF NOT EXISTS routing_prediction_scores (
    id             TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    prediction_id  TEXT             NOT NULL UNIQUE,         -- score-once per prediction (routing_predictions.id)
    slice_size     INTEGER          NOT NULL,                -- |held slice| actually scored (≥ MinSliceSize, ≤ SliceCap)
    m_avg          DOUBLE PRECISION NOT NULL,                -- avg eval.StaticScore of the predicted model M on the slice
    baseline_avg   DOUBLE PRECISION NOT NULL,                -- avg eval.StaticScore of the baseline default route on the SAME slice
    baseline_model TEXT             NOT NULL,                -- the Advisor's cohort pick that M was measured against (audit)
    skill_margin   DOUBLE PRECISION NOT NULL,                -- clamp01(m_avg − baseline_avg) — what PR-4 mints on
    scored_at      TIMESTAMPTZ      NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_routing_prediction_scores_prediction
    ON routing_prediction_scores (prediction_id);
