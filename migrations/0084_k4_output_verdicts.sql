-- 0084_k4_output_verdicts.sql
-- K4 per-output INTRINSIC verdict store. One row per served output, keyed by the SERVER-DERIVED,
-- gateway-bound output_id (outputverify.DeriveOutputID) — a workspace can neither forge nor repudiate it.
--
-- INTRA-TENANT ONLY: workspace_id is SELF (no counterparty column, mirroring keel_findings). There is NO
-- cross-tenant read, no cohort, no comparison anywhere that touches this table.
--
-- HASHES ONLY: prompt_sha256 / response_sha256 are hex(sha256(...)). The raw prompt/response TEXT is NEVER
-- stored — a raw-content table is out of scope and must not exist.
--
-- Append-only: INSERT ON CONFLICT (output_id) DO NOTHING. This table carries a VERDICT, never money —
-- nothing mints or slashes from it in this run.
--
-- verdict is a SQL-ENFORCEABLE ENUM. 'unverifiable' (the majority) means "no machine-checkable constraint
-- was declared"; it is a DISTINCT value precisely so a bond can NEVER be slashable on "we couldn't check
-- it". Only 'failed_constraint' is a violation.
CREATE TABLE IF NOT EXISTS k4_output_verdicts (
    output_id       TEXT PRIMARY KEY,             -- server-derived, gateway-bound identity (sha256 hex)
    workspace_id    TEXT NOT NULL,                -- SELF only — never a counterparty
    model           TEXT NOT NULL,
    verdict         TEXT NOT NULL CHECK (verdict IN ('passed', 'failed_constraint', 'unverifiable')),
    reason          TEXT NOT NULL DEFAULT '',     -- machine-readable, only when verdict='failed_constraint'
    constraint_kind TEXT NOT NULL DEFAULT 'none', -- json_object | json_schema | tool_call | none
    prompt_sha256   TEXT NOT NULL,                -- hex(sha256(prompt)) — HASH ONLY, never raw text
    response_sha256 TEXT NOT NULL,                -- hex(sha256(response)) — HASH ONLY, never raw text
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_k4_output_verdicts_ws
    ON k4_output_verdicts (workspace_id, created_at DESC);
