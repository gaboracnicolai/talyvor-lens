-- 0074_node_cohort_latency_stats.sql — Proof-of-latency-locality, the DESCRIPTIVE capture (P3 #6, PR L1+L2).
--
-- A rolling per-(node, cohort) latency aggregate — the substrate a LATER mint (PR-L4) reads to pay a node
-- for genuinely-fast service, cohort-relative + cost-weighted + quality-gated (the quality gate is the
-- existing benchmark_node_scores, node-blind held probes). This migration is MINT-FREE: it only adds the
-- aggregate; nothing here credits the ledger. Written OFF the serve path via the obsLimiter (best-effort,
-- shed under load), gated behind a default-off capability flag.
--
-- Composition (ii): store the RAW signals — a decayed EWMA of gateway-measured serve latency + a
-- cost-weight accumulator (Σ AnalyseComplexity.Score() over samples) — and let the mint compose the
-- cost-weighted cohort-relative score. Do NOT pre-normalize here (freeze the interface, not the impl).
-- cohort = (feature_category [client-declared], input_token_range + complexity_bucket [input-derived]).
CREATE TABLE IF NOT EXISTS node_cohort_latency_stats (
    node_id           TEXT             NOT NULL,             -- the serving node (inference_nodes.id)
    feature_category  TEXT             NOT NULL,             -- cohort dim 1 (X-Talyvor-Feature, client-declared)
    input_token_range TEXT             NOT NULL,             -- cohort dim 2 (input-derived)
    complexity_bucket TEXT             NOT NULL,             -- cohort dim 3 (input-derived)
    latency_ewma      DOUBLE PRECISION NOT NULL DEFAULT 0,   -- decayed EWMA (α=0.2) of gateway-measured serve latency (ms)
    cost_weight_accum DOUBLE PRECISION NOT NULL DEFAULT 0,   -- Σ AnalyseComplexity(prompt).Score() over samples (A: cohort cost context)
    sample_count      BIGINT           NOT NULL DEFAULT 0,   -- observations folded into this row
    updated_at        TIMESTAMPTZ      NOT NULL DEFAULT now(),
    PRIMARY KEY (node_id, feature_category, input_token_range, complexity_bucket)
);
