-- 0097_output_attributions.sql
-- Output attribution (K4) — DESCRIPTIVE, MONEY-FREE. The PRODUCING workspace attributes an output
-- it OWNS (present in k4_output_verdicts under its workspace_id) to a PR or spec. Attribution is
-- NOT settlement: this table has NO amount column, NO ledger/mint/held/bond linkage, and the write
-- path never reads, writes, sums, or rounds a µLENS amount. target_ref is an OPAQUE free string —
-- Lens never parses it and never dereferences it (no GitHub call).
--
-- Migration NUMBER: sequential against main's high-water 0096 → 0097. No collision (verified at
-- write time against origin/main).
--
-- Append-only, first-wins per (output_id, workspace_id, target_kind): the PRIMARY KEY carries every
-- identity it protects (SEC-11) — a different tenant with a colliding output_id/kind cannot overwrite,
-- and a conflicting re-attribution for the same kind is refused by the writer (409), never mutated.
-- Ownership is enforced in the WRITE (an EXISTS gate against k4_output_verdicts, mirroring
-- k4_mechanical_verdicts) — NOT a foreign key — so this table stands alone.
CREATE TABLE IF NOT EXISTS output_attributions (
    output_id    TEXT NOT NULL,
    workspace_id TEXT NOT NULL,
    target_kind  TEXT NOT NULL,
    target_ref   TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (output_id, workspace_id, target_kind)
);
