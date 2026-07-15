-- 0095_model_capability_observations.sql
-- Model-capability curves (H2). A DESCRIPTIVE, per-model record of observed
-- quality at a work-tier DIFFICULTY derived from H1's WorkTier classification
-- (the size + complexity hardness axes). The read side (internal/modelcapability
-- Fit) fits a per-model quality-vs-difficulty trend — how a model's quality HOLDS
-- as work-tier rises. Synthetic-buildable: seeded representative traffic, no
-- live-traffic dependency.
--
-- Migration NUMBER: sequential against main's high-water 0093. Deliberately 0095,
-- NOT 0094 — a concurrent sibling session holds 0094 (k4_artifact_commitment) in
-- its own branch; taking 0095 guarantees no duplicate-version collision when both
-- merge (the runner errors on duplicate versions but tolerates gaps).
--
-- DESCRIPTIVE, NEVER INCENTIVIZED: a measured capability is analytics, never a
-- reward multiplier (the descriptive-never-incentivized doctrine; see WorkTier /
-- migration 0059). NON-CONTENT columns ONLY — model identity, the H1 difficulty
-- ordinal, and the composite quality score. Default-off capability (no serve-path
-- wiring; seeded synthetically), mint-free by construction (the store holds only
-- an Exec/Query handle — no ledger).
CREATE TABLE IF NOT EXISTS model_capability_observations (
    id          BIGSERIAL PRIMARY KEY,
    model       TEXT NOT NULL,
    provider    TEXT NOT NULL DEFAULT '',
    difficulty  INTEGER NOT NULL,          -- H1 work-tier ordinal (size+complexity), [0,6]
    quality     DOUBLE PRECISION NOT NULL, -- composite quality score [0,1]
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- The curve fit scans grouped by (model, provider, difficulty).
CREATE INDEX IF NOT EXISTS idx_model_capability_model
    ON model_capability_observations (model, provider, difficulty);
