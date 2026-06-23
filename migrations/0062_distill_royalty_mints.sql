-- 0062_distill_royalty_mints.sql — L2/S4 PR3: the gated distill reuse-royalty
-- CLAIM/MINT record. A SEPARATE table from pool_royalty_mints, deliberately:
-- distill rows mixed into pool_royalty_mints would contaminate the cache
-- collusion DETECTOR, the per-pair/per-entry mint CAPS, the MARGIN view, and the
-- resolver — all of which scan pool_royalty_mints with NO layer filter. This
-- table carries NO cache-only columns (no layer/provider/model/similarity/
-- answer+prompt hashes). It follows the established distill-owns-its-tables
-- pattern (distill_serve_attribution 0052, distill_royalty_basis 0061).
--
-- ONE ROW PER cross-tenant reuse RELATIONSHIP (owner A, requester B, content
-- hash). request_id = SHA256Hex(owner:requester:content_hash) UNIQUE is the
-- once-per-relationship idempotency key: the distill mint sweeper claims it ON
-- CONFLICT (request_id) DO NOTHING and credits the contributor's HELD balance
-- ONLY when the insert took a NEW row (RowsAffected == 1). Claim + credit join
-- ONE transaction, so a re-run / leader race / retry credits A exactly once (the
-- second insert no-ops → no credit), and an unverified-A credit (U6 floor) rolls
-- the WHOLE tx back — the claim row is discarded and the relationship stays
-- un-minted, re-eligible once A verifies (the floor DELAYS, never forfeits).
--
-- LIFECYCLE mirrors pool_royalty_mints: status held → final (the parameterized
-- FinalizeSweeper settles rows past finalize_after via the SHARED FinalizeHeldTx
-- kernel — so supply counts at FINALIZE via the counted TypePoolRoyalty ledger
-- row, never at the held credit) → revoked. Revoke/adjudication + a distill
-- detector/per-pair caps are DEFERRED to public-go-live; meanwhile the U6
-- verified-floor (on the credited owner A) + the per-identity rate cap + the
-- card-fingerprint linkage are the closed test. Unpartitioned. DOUBLE PRECISION
-- matches pool_royalty_mints / distill_royalty_basis (COGS estimate, not a
-- settled balance). Plain transactional CREATE TABLE.

CREATE TABLE IF NOT EXISTS distill_royalty_mints (
    id                       UUID             PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id               TEXT             NOT NULL UNIQUE,          -- SHA256Hex(owner:requester:content_hash): once-per-relationship
    contributor_workspace_id TEXT             NOT NULL,                 -- owner A (the OCR contributor) — the CREDITED workspace
    requester_workspace_id   TEXT             NOT NULL,                 -- reuser B (admin-only; NEVER named in A's ledger row, #145)
    content_hash             TEXT             NOT NULL,                 -- distill.ContentHash(raw document bytes)
    avoided_cogs_usd         DOUBLE PRECISION NOT NULL,                 -- the PINNED PR2 basis snapshot
    minted_amount            DOUBLE PRECISION NOT NULL,                 -- s × avoided_cogs_usd (s = cfg.PoolRoyaltyShare)
    status                   TEXT             NOT NULL DEFAULT 'held',  -- held | final | revoked
    finalize_after           TIMESTAMPTZ,                              -- now + 72h holdback; the finalize sweeper settles rows past this
    created_at               TIMESTAMPTZ      NOT NULL DEFAULT NOW()
);

-- The finalize sweeper's access path: held rows due for settlement.
CREATE INDEX IF NOT EXISTS idx_distill_royalty_mints_finalize
    ON distill_royalty_mints (finalize_after) WHERE status = 'held';

-- The mint sweeper's un-minted scan: distill_royalty_basis LEFT JOIN here on the
-- relationship key WHERE no claim exists yet (leaves 0061 untouched).
CREATE INDEX IF NOT EXISTS idx_distill_royalty_mints_relationship
    ON distill_royalty_mints (contributor_workspace_id, requester_workspace_id, content_hash);
