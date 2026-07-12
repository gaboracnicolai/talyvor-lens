-- 0081_keel_findings_mode.sql
-- U25 K3 — money-grade HARDENED findings need to be distinguishable from ordinary findings at the SQL
-- layer, so H5 (provenance bonds) may read ONLY hardened findings and can never accidentally slash on an
-- ordinary (non-hardened, contaminable-mean) finding.
--
-- A `mode` column is used INSTEAD of an identity_key prefix: identity_key is a sha256 HEX digest, so its
-- pre-hash prefix ("keel:" vs "keelh:") is NOT recoverable in SQL — a WHERE/LIKE could not filter on it.
-- A `mode` column makes "hardened only" a trivial, indexable, enforceable predicate: WHERE mode='hardened'.
--
-- Additive + non-behaviour-changing: existing rows and the existing ordinary write path are unaffected
-- (DEFAULT 'ordinary' backfills every current row and every future ordinary insert).
ALTER TABLE keel_findings ADD COLUMN IF NOT EXISTS mode TEXT NOT NULL DEFAULT 'ordinary';

-- Partial index for H5's hardened-only reads (keeps the ordinary hot path's index unchanged).
CREATE INDEX IF NOT EXISTS idx_keel_findings_hardened
    ON keel_findings (workspace_id, unit, window_bucket)
    WHERE mode = 'hardened';
