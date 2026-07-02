-- 0078_confidential_compute_mints.sql — Proof-of-Improvement instance 4: proof-of-confidential-compute (the
-- CONFIDENTIAL COMPUTE MINT). Rewards a NODE for providing VERIFIED confidential capacity — the scarce,
-- cryptographically-verified property — through the SAME held-ledger / U6 chokepoint as every other mint.
-- The 4th live HeldBenchmarkAnchor caller. Epoch-settled once per (node, attested_gpu_class, 24h window).
--
-- WHAT IT PAYS: verified confidential capacity, NOT latency/volume. The latency mint (0076) already pays
-- latency; re-paying it here would double-count. The reward is FLAT per eligible (node, class, epoch) —
-- rate × 1.0 via the anchor. attested_gpu_class is the gateway-VERIFIED class (from the NVIDIA EAT), never
-- the node-declared gpu_type.
--
-- ELIGIBILITY (all AND-ed, in the minter's scan): (i) a node_attestations row status='verified' AND
-- key_bound=true AND expires_at > now() — key_bound=true is the RELAY FENCE, so relay-vulnerable rows are
-- excluded here; (ii) held-probe correctness — benchmark_node_scores.score >= threshold AND sample_count >=
-- warmup (reuse the latency mint's C-gate). The mint NEVER reads gpu_type or any latency signal.
--
-- INERT: a mint requires LENS_CONFIDENTIAL_MINTING_ENABLED + LENS_PROOF_OF_IMPROVEMENT_ENABLED on AND a
-- positive LENS_CONFIDENTIAL_RATE_PER_POINT. It is ALSO inert-by-substrate-absence: no key_bound=true row
-- exists until real CC hardware + enclave report_data binding is deployed, so RunOnce mints ZERO even flag+
-- rate on. This migration only adds the claim; it touches no ledger/economy/node_attestations table.
CREATE TABLE IF NOT EXISTS confidential_compute_mints (
    request_id               TEXT             PRIMARY KEY, -- SHA256Hex(node_id:attested_gpu_class:epoch) — once-per-window
    contributor_workspace_id TEXT             NOT NULL,    -- the serving node's workspace (inference_nodes.workspace_id) — the payee
    minted_amount            DOUBLE PRECISION NOT NULL,    -- rate × 1.0 (flat verified-capacity reward) — what CreditHeldTx credited
    node_id                  TEXT             NOT NULL,    -- audit / dedup key
    attested_gpu_class       TEXT             NOT NULL,    -- audit: the gateway-VERIFIED hardware class (never gpu_type)
    epoch                    BIGINT           NOT NULL,    -- floor(unixtime / windowSeconds)
    status                   TEXT             NOT NULL DEFAULT 'held',
    finalize_after           TIMESTAMPTZ      NOT NULL,
    created_at               TIMESTAMPTZ      NOT NULL DEFAULT NOW()
);

-- The partial index the generic FinalizeSweeper SELECT rides (held rows past their finalize time).
CREATE INDEX IF NOT EXISTS idx_confidential_compute_mints_finalize
    ON confidential_compute_mints (finalize_after) WHERE status = 'held';

-- Backs the minter's "already claimed this (node,class,epoch)?" NOT EXISTS filter.
CREATE INDEX IF NOT EXISTS idx_confidential_compute_mints_dedup
    ON confidential_compute_mints (node_id, attested_gpu_class, epoch);
