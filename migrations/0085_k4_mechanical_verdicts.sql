-- 0085_k4_mechanical_verdicts.sql
-- CODE LOOP: a MECHANICAL verdict (did it COMPILE / did TESTS PASS) for a K4 gateway-bound output_id,
-- SELF-REPORTED by the producing workspace (talyvor-code closes the loop: exit code 0 = the least-arguable
-- correctness signal Talyvor has).
--
-- OWNERSHIP BINDING: keyed to output_id, which is derived from (workspace_id ‖ model ‖ prompt_hash ‖
-- response_hash ‖ served_at_bucket) — see internal/outputverify. A workspace can therefore ONLY report on an
-- output_id it produced; the write path (RecordMechanicalIfOwned) inserts ONLY WHERE EXISTS a
-- k4_output_verdicts row with this output_id AND workspace_id = the caller. Workspace B cannot report on
-- workspace A's output_id.
--
-- ⚠ TRUST MODEL — verdict_source = 'self_reported': a self-reported FAILURE (compile_failed / tests_failed)
-- is credible AGAINST INTEREST (nobody falsely confesses to being slashable), so it is usable as H5 slash
-- evidence. A self-reported PASS (compiled / tests_passed) proves NOTHING (a liar always claims success) and
-- MUST NEVER release a bond by itself. The verdict_source column exists so H5 can never confuse a
-- self-reported pass with an attested one; a trusted/attested runner is a FUTURE verdict_source, NOT built
-- here.
--
-- Append-only, FIRST-REPORT-WINS: PRIMARY KEY (output_id, verdict_source) + INSERT ON CONFLICT DO NOTHING —
-- no mutation, no overwrite, no replay. HASHES ONLY (no raw content column). SELF workspace only (no
-- counterparty column). This table carries a VERDICT, never money.
CREATE TABLE IF NOT EXISTS k4_mechanical_verdicts (
    output_id      TEXT NOT NULL,                            -- the gateway-bound identity (0084)
    workspace_id   TEXT NOT NULL,                            -- SELF only — the producing workspace (owns output_id)
    verdict        TEXT NOT NULL CHECK (verdict IN ('compiled', 'compile_failed', 'tests_passed', 'tests_failed')),
    exit_code      INTEGER NOT NULL,
    tool           TEXT NOT NULL DEFAULT '',                 -- e.g. 'go build' | 'go test'
    reason         TEXT NOT NULL DEFAULT '',
    verdict_source TEXT NOT NULL DEFAULT 'self_reported',    -- 'self_reported' now; 'attested' is future hardening
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (output_id, verdict_source)
);

CREATE INDEX IF NOT EXISTS idx_k4_mech_verdicts_ws
    ON k4_mechanical_verdicts (workspace_id, created_at DESC);
