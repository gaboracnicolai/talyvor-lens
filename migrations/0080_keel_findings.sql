-- 0080_keel_findings.sql — the append-only forensic record for KEEL (U25) cross-tenant DRIFT ATTRIBUTION.
-- Mirrors 0065_royalty_detector_findings' discipline: the leader-elected keel sweep INSERTs one row per
-- flagged drift finding; APPEND-ONLY by construction — the writer only ever runs
-- `INSERT … ON CONFLICT (identity_key) DO NOTHING`, never UPDATE/DELETE, so a re-sweep never duplicates or
-- mutates a finding. identity_key = sha256(keel:<workspace>:<unit>:<window>) dedups a flag across sweeps.
--
-- TENANCY BOUNDARY (structural): a row names ONLY the SELF workspace_id. There is DELIBERATELY no
-- counterparty column and no raw cross-tenant quality — cross-tenant values existed only as the cohort
-- aggregate (mean/stddev over ≥ MinWorkspaces distinct workspaces) that produced deviation_sigma. The
-- read surface is requireAdmin-gated (a tenant must never read another tenant's drift attribution).
-- NOT part of the token economy: no ledger, no supply, no held balance — Keel never mints or acts.
CREATE TABLE IF NOT EXISTS keel_findings (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    TEXT NOT NULL,             -- SELF only (never a counterparty)
    unit            TEXT NOT NULL,             -- comparison unit: provider_used/model_used
    window_bucket   BIGINT NOT NULL,           -- the current window bucket (epoch/window_seconds)
    deviation_sigma DOUBLE PRECISION NOT NULL, -- self residual vs cohort, in cohort-stddev units
    attribution     TEXT NOT NULL,             -- 'idiosyncratic' | 'common_mode'
    cohort_n        INTEGER NOT NULL,          -- distinct opted-in workspaces in the cohort (>= MinWorkspaces)
    identity_key    TEXT NOT NULL UNIQUE,      -- sha256(keel:<workspace>:<unit>:<window>) — dedup across sweeps
    metrics         JSONB NOT NULL,            -- full numeric evidence (cohort_mean, cohort_stddev, residual_shift…)
    first_seen_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_keel_findings_ws
    ON keel_findings (workspace_id, first_seen_at DESC);
