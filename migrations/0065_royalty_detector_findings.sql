-- 0065_royalty_detector_findings.sql — the durable forensic record for the scheduled
-- royalty detector sweep (the "smoke detector"). The leader-elected DetectorSweep runs
-- the cache + distill fraud detectors on a cadence and INSERTs one row per FLAGGED
-- finding here. APPEND-ONLY by construction: the writer only ever runs
-- `INSERT … ON CONFLICT (identity_key) DO NOTHING` — never UPDATE/DELETE — so re-sweeps
-- don't duplicate a finding and an existing row is never mutated. identity_key dedups a
-- distinct flag across sweeps (sha256 of economy:detector:<the detector's identity fields>).
-- This table is OBSERVABILITY, not a money table — nothing here moves LENS; the sweep
-- never resolves/adjudicates (the never-auto-act invariant). Read it admin-gated, like
-- the /detect endpoints.

CREATE TABLE IF NOT EXISTS royalty_detector_findings (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    economy                  TEXT NOT NULL,            -- 'cache' | 'distill'
    detector                 TEXT NOT NULL,            -- 'volume' | 'bilateral' | 'similarity'
    identity_key             TEXT NOT NULL UNIQUE,     -- sha256(economy:detector:<identity fields>) — dedup across sweeps
    contributor_workspace_id TEXT NOT NULL,
    requester_workspace_id   TEXT,                     -- volume/bilateral; NULL where the detector has no single requester
    entry_or_content         TEXT,                     -- entry_id (cache) / content_hash (distill); NULL for bilateral
    window_seconds           BIGINT NOT NULL,          -- the rolling detection window the sweep used
    metrics                  JSONB NOT NULL,           -- the full flag's numeric evidence (mints, fracs, similarity stats…)
    first_seen_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Read patterns: "what's flagged for this contributor / economy", newest first.
CREATE INDEX IF NOT EXISTS idx_royalty_detector_findings_lookup
    ON royalty_detector_findings (economy, detector, first_seen_at DESC);
