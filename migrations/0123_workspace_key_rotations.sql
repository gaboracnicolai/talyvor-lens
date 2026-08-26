-- 0123_workspace_key_rotations.sql — W1.9.1: a rotation a RUNNING SERVICE can survive.
--
-- THE DEFECT THIS EXISTS FOR. `tenant.Store.RotateAPIKey` is one transaction:
-- SELECT FOR UPDATE the old key → INSERT the new one → DELETE the old one. Its own
-- comment says "INSERT precedes DELETE so the workspace is never left with zero
-- active keys mid-transaction", and that is true and is about the WORKSPACE. The
-- HOLDER is a different party: at commit the key a running service is presenting
-- stops authenticating, and the service has no moment in which to switch. The
-- endpoint named `rotate` is therefore an outage by construction — which is
-- exactly W1.9.1's finding, and why an OVERLAP WINDOW is the feature rather than
-- a nicety.
--
-- WHAT THIS TABLE IS. One row per rotation in flight: the old key, the new key,
-- and when the rotation began. Both keys are live for as long as the row is open,
-- so the holder can switch at its own pace. The row is what makes the sequence
-- resumable — a rotation that a deploy interrupts is still on record, and both
-- keys still work, rather than being an outage nobody can reconstruct.
--
-- ⚠ started_at IS LOAD-BEARING, NOT AUDIT DECORATION. Completion is allowed only
-- when the NEW key's workspace_api_keys.last_used_at is AFTER this instant — that
-- is, when the new key has actually authenticated a real request. A NULL
-- last_used_at fails that comparison, and so does a timestamp from before the
-- rotation opened, so neither a fresh key nor a key that happened to be used
-- earlier can be mistaken for proof. W1.9.1's instruction is "verify with an
-- authenticated request, never a 200 from an unauthenticated health route"; this
-- column is how that becomes structural instead of a habit.
--
-- UNIQUE (old_key_id) WHERE completed_at IS NULL: one OPEN rotation per key. Two
-- concurrent begins on one key would mint two replacements and leave the operator
-- unable to say which one the holder took. A completed rotation does not block a
-- later one, so a key may be rotated repeatedly over its life.
--
-- Additive: own file, own table, no existing table or query touched. Nothing reads
-- it until the routes added in this same merge are called.

CREATE TABLE IF NOT EXISTS workspace_key_rotations (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  TEXT NOT NULL,
    old_key_id    UUID NOT NULL,
    new_key_id    UUID NOT NULL,
    started_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at  TIMESTAMPTZ,
    -- 'completed' | 'abandoned'. NULL while the rotation is open.
    outcome       TEXT
);

-- One OPEN rotation per key. Partial, so history does not block a later rotation.
CREATE UNIQUE INDEX IF NOT EXISTS workspace_key_rotations_one_open_per_key
    ON workspace_key_rotations (old_key_id) WHERE completed_at IS NULL;

CREATE INDEX IF NOT EXISTS workspace_key_rotations_by_workspace
    ON workspace_key_rotations (workspace_id, started_at DESC);
