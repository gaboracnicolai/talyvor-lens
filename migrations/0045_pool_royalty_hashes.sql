-- 0045_pool_royalty_hashes.sql — Phase-2 Stage 2.3.0: serve-time evidence
-- hashes on the Pool-B royalty claim row, so pooled mints are later
-- ADJUDICABLE (poisoning: did this answer deserve a royalty?) and
-- FORENSICALLY CORRELATABLE (similarity-gaming, self-dealing, volume
-- patterns).
--
--   answer_sha256 = hex(sha256(served response bytes))
--   prompt_sha256 = hex(sha256(raw requester prompt bytes))
--
-- Both are UNSALTED pure content hashes (no provider:model prefix — the
-- salted identities already live on the row via entry_id), computed AT THE
-- MOMENT OF SERVE in the same single mint transaction as the claim insert.
-- Serve-time capture is load-bearing: both cache stores are mutable
-- underneath the mint (the exact cache is a Redis SET with a TTL; the pooled
-- semantic row's response is overwritten by ON CONFLICT DO UPDATE on
-- re-contribution), so a lazily-computed hash could hash different bytes
-- than what was served. The recorded hash makes a later entry overwrite —
-- including a contributor destroying evidence of their own poisoned entry —
-- TAMPER-EVIDENT: recorded hash ≠ current entry hash proves the entry
-- changed since the serve.
--
-- DEFAULT '' = "not captured" — pre-2.3.0 rows keep working (historical,
-- legitimate). For NEW serves the write path enforces no-hash → no-mint
-- (the privacy-coherence rule): a serve whose hashes cannot be captured
-- (e.g. a none-LoggingPolicy requester) still serves and caches normally
-- but writes NO claim row and mints NOTHING — an unadjudicable mint is
-- never created. That gate lives in code (MintServedHit), never as a scan
-- over existing rows, so historical '' rows can never trip it.
--
-- Additive, own-file, idempotent. Touches no index, no view, no lock.

ALTER TABLE pool_royalty_mints
    ADD COLUMN IF NOT EXISTS answer_sha256 TEXT NOT NULL DEFAULT '';

ALTER TABLE pool_royalty_mints
    ADD COLUMN IF NOT EXISTS prompt_sha256 TEXT NOT NULL DEFAULT '';
