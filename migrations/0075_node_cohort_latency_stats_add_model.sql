-- 0075_node_cohort_latency_stats_add_model.sql — widen the node-latency cohort grain to include `model`
-- (P3 #6, PR L3.5). The L4 mint's quality gate is benchmark_node_scores, which is per-(node, MODEL); to
-- gate C exactly (not a per-node aggregate across models) the latency aggregate must also carry `model`.
--
-- The table is DESCRIPTIVE + MINT-FREE and, by construction, EMPTY at this point: the capture flag
-- (LENS_NODE_LATENCY_CAPTURE_ENABLED) is default-off and 0074 only just landed, so no rows have been
-- captured. This migration DROPs + re-CREATEs with the wider 5-col PK rather than ALTERing.
--
-- MANDATORY GUARD: convert "the table is empty" from an assumption into an ENFORCED precondition — fail
-- loud (abort the migration) if ANY row exists, so a deliberate data migration is required should capture
-- ever have been switched on. Touches ONLY node_cohort_latency_stats — no ledger/mint/economy table.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM node_cohort_latency_stats LIMIT 1) THEN
        RAISE EXCEPTION 'node_cohort_latency_stats not empty — L3.5 DROP+CREATE would lose captured rows; migrate deliberately';
    END IF;
END $$;

DROP TABLE IF EXISTS node_cohort_latency_stats;

CREATE TABLE node_cohort_latency_stats (
    node_id           TEXT             NOT NULL,             -- the serving node (inference_nodes.id)
    feature_category  TEXT             NOT NULL,             -- cohort dim 1 (X-Talyvor-Feature, client-declared)
    input_token_range TEXT             NOT NULL,             -- cohort dim 2 (input-derived)
    complexity_bucket TEXT             NOT NULL,             -- cohort dim 3 (input-derived)
    model             TEXT             NOT NULL,             -- cohort dim 4 (the served model — aligns C to benchmark_node_scores' (node,model) grain)
    latency_ewma      DOUBLE PRECISION NOT NULL DEFAULT 0,   -- decayed EWMA (α=0.2) of gateway-measured serve latency (ms)
    cost_weight_accum DOUBLE PRECISION NOT NULL DEFAULT 0,   -- Σ AnalyseComplexity(prompt).Score() over samples (A: cohort cost context)
    sample_count      BIGINT           NOT NULL DEFAULT 0,   -- observations folded into this row
    updated_at        TIMESTAMPTZ      NOT NULL DEFAULT now(),
    PRIMARY KEY (node_id, feature_category, input_token_range, complexity_bucket, model)
);
