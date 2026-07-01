-- 0076_node_latency_mints.sql — Proof-of-Improvement instance 3: proof-of-latency-locality (the LATENCY MINT).
--
-- Rewards a NODE for genuinely-fast service — cohort-relative + cost-weighted + quality-gated — paid on the
-- per-(node,cohort,model) latency EWMA the descriptive capture (node_cohort_latency_stats, PRs L1/L2/L3.5)
-- already produced, through the SAME held-ledger / U6 chokepoint as every other mint. It is the THIRD live
-- caller of HeldBenchmarkAnchor. Unlike the two P-o-I mints (per-event, request_id-keyed), this is the FIRST
-- EPOCH-SETTLED mint: a node is paid at most once per (node,cohort,model,epoch) window.
--
-- THE READOUT (anti-farming): the baseline is a PER-CANDIDATE median of latency_ewma over nodes
-- fingerprint-UNLINKED to that candidate (the same workspace_card_fingerprints self-deal exclusion the
-- royalty / eval-contribution / routing paths use) — so a workspace cannot inject slow linked nodes to
-- lower its own bar. A cohort needs >= MinUnlinkedNodes distinct unlinked workspaces or it pays nothing.
-- The quality gate is EXACT per-(node,model) (benchmark_node_scores, node-blind held probes) — fast-and-wrong
-- is closed because a node cannot distinguish a probe from live traffic. Latency is GATEWAY-measured, so it
-- is not node-asserted; the aggregate is per-(node,cohort,model), never keyed on a node-supplied request_id.
--
-- INERT: a mint requires LENS_LATENCY_MINTING_ENABLED + LENS_PROOF_OF_IMPROVEMENT_ENABLED on AND a positive
-- LENS_LATENCY_RATE_PER_POINT. Default rate 0 ⇒ the anchor refuses ⇒ no mint. This migration only adds the
-- once-per-(node,cohort,model,epoch) claim; it touches no ledger/capture/economy table.

-- The epoch-settled mint claim — mirrors routing_prediction_mints / eval_contribution_mints for the generic
-- (request_id, contributor_workspace_id, minted_amount, status, finalize_after) finalize columns so the
-- generic FinalizeSweeper settles it UNCHANGED. request_id = SHA256Hex(node:feature:itr:complexity:model:epoch)
-- (a deterministic composite — the distill_royalty_mints once-per-relationship pattern) = the once-per-window
-- idempotency key. The audit columns (node_id + the 4 cohort dims + epoch) also back the not-yet-claimed
-- LEFT JOIN the sweep uses to skip rows already settled this epoch.
CREATE TABLE IF NOT EXISTS node_latency_mints (
    request_id               TEXT             PRIMARY KEY, -- SHA256Hex(node_id:feature_category:input_token_range:complexity_bucket:model:epoch)
    contributor_workspace_id TEXT             NOT NULL,    -- the serving node's workspace (inference_nodes.workspace_id) — the payee
    latency_skill            DOUBLE PRECISION NOT NULL,    -- the held-score paid on: clamp01((baseline-L)/baseline) × (cohortCost/maxCost)
    minted_amount            DOUBLE PRECISION NOT NULL,    -- rate × latency_skill (what CreditHeldTx credited)
    node_id                  TEXT             NOT NULL,    -- audit + the not-yet-claimed-this-epoch LEFT JOIN key
    feature_category         TEXT             NOT NULL,    -- audit / cohort dim 1
    input_token_range        TEXT             NOT NULL,    -- audit / cohort dim 2
    complexity_bucket        TEXT             NOT NULL,    -- audit / cohort dim 3
    model                    TEXT             NOT NULL,    -- audit / cohort dim 4
    epoch                    BIGINT           NOT NULL,    -- floor(unixtime / windowSeconds) — the settlement window
    status                   TEXT             NOT NULL DEFAULT 'held',
    finalize_after           TIMESTAMPTZ      NOT NULL,
    created_at               TIMESTAMPTZ      NOT NULL DEFAULT NOW()
);

-- The partial index the generic FinalizeSweeper SELECT rides (held rows past their finalize time).
CREATE INDEX IF NOT EXISTS idx_node_latency_mints_finalize
    ON node_latency_mints (finalize_after) WHERE status = 'held';

-- Backs the minter's "already claimed this (node,cohort,model,epoch)?" LEFT JOIN so a re-scan within an
-- epoch skips settled rows and the bounded batch advances to fresh candidates.
CREATE INDEX IF NOT EXISTS idx_node_latency_mints_epoch
    ON node_latency_mints (node_id, feature_category, input_token_range, complexity_bucket, model, epoch);
