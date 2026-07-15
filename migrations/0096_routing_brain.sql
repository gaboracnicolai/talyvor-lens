-- 0096_routing_brain.sql
-- H8.1 Routing Brain — DESCRIPTIVE, MINT-FREE. The brain learns OFFLINE from
-- verified routing outcomes (H2 capability curves + Keel drift, keyed by the H1/H2
-- work-tier difficulty) and writes a per-(workspace, difficulty) best-model
-- recommendation. The serving path READS the latest precomputed recommendation and
-- either SURFACES it (advisory, the default) or APPLIES it within the hard floor
-- (autonomous, explicit per-workspace opt-in). It NEVER mints, debits, or writes a
-- ledger row (import-guarded; the store holds only Exec/Query seams).
--
-- Migration NUMBER: sequential against main's high-water 0095 → 0096. No collision.
--
-- Two tables:
CREATE TABLE IF NOT EXISTS routing_brain_recommendations (
    workspace_id     TEXT NOT NULL,
    difficulty       INTEGER NOT NULL,                    -- H1/H2 work-tier ordinal [0,6]
    model            TEXT NOT NULL,
    provider         TEXT NOT NULL DEFAULT '',
    expected_quality DOUBLE PRECISION NOT NULL DEFAULT 0, -- capability estimate [0,1] (H2 curve)
    verified         BOOLEAN NOT NULL DEFAULT FALSE,      -- passed Keel-drift + allow-list at compute time
    reason           TEXT NOT NULL DEFAULT '',
    computed_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (workspace_id, difficulty)               -- one current pick per cohort
);

-- The per-workspace autonomous opt-in. PRESENCE = the workspace trusts the brain to
-- APPLY its pick (within the hard floor); ABSENCE = advisory (the default).
CREATE TABLE IF NOT EXISTS routing_brain_autonomous (
    workspace_id TEXT PRIMARY KEY,
    opted_in_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
