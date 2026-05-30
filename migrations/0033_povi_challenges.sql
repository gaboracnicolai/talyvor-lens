-- 0033_povi_challenges.sql — challenge-and-slash audit + the leaf-count hint
-- (PoVI Token Economy Phase 1, Part 3 / Master Plan Upgrade 4).
--
-- Part 3 closes the security loop: Lens randomly challenges a node to produce
-- Merkle authentication paths for sampled positions in a receipt's committed
-- trace; failure (bad path or no answer) slashes the node's stake (Part 2),
-- making receipt-minting economically safe (Part 1's attestation alone is not).
--
-- Two changes:
--
--   1. povi_receipts.leaf_count — the number of leaves in the committed trace
--      (output runes). An UNSIGNED hint (not part of the signed receipt) so Lens
--      knows the sampling range [0, leaf_count) for a challenge. A node can't
--      benefit from lying: sampled positions must still verify against the
--      SIGNED merkle_root.
--
--   2. povi_challenges — the audit trail of every challenge + its outcome.
--      One challenge per receipt (request_id UNIQUE) is the double-slash guard:
--      a single failed receipt can be slashed at most once.

ALTER TABLE povi_receipts
    ADD COLUMN IF NOT EXISTS leaf_count INTEGER NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS povi_challenges (
    id             TEXT PRIMARY KEY,
    request_id     TEXT NOT NULL UNIQUE,                 -- one challenge per receipt (double-slash guard)
    node_id        TEXT NOT NULL,
    workspace_id   TEXT NOT NULL,
    positions      TEXT NOT NULL DEFAULT '',             -- comma-joined sampled positions
    result         TEXT NOT NULL,                        -- pass | fail | timeout
    slashed_amount DOUBLE PRECISION NOT NULL DEFAULT 0,  -- LENS burned on failure
    reason         TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_povi_challenges_node ON povi_challenges(node_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_povi_challenges_result ON povi_challenges(result);
