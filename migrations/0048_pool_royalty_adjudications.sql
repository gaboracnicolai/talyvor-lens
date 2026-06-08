-- 0048_pool_royalty_adjudications.sql — Phase-2 Stage 3: the pool-mint
-- ADJUDICATION record — the audit row that BINDS record→revoke.
--
-- This is the first migration of the gate that connects detect → resolve →
-- revoke through a DELIBERATE operator decision. The never-auto-act invariant
-- is structural (an authenticated admin must pass an explicitly-chosen subset
-- of request_ids; nothing auto-selects), and THIS table makes the decision
-- DURABLE: the adjudication row is written BEFORE the held-burn fires, so a
-- production revoke can never happen without a preceding audit record.
--
-- WHY this table and not povi_challenges: that table is receipt/node/Merkle/
-- stake-shaped (one workspace, Merkle positions, slash of locked collateral).
-- A pool mint is claim-row/two-workspace/content-hash/held-balance-shaped, so
-- every load-bearing column would be a forced fit. This record is purpose-built.
--
-- COLUMNS:
--   flag_type / resolution_label — the BASIS: which detector flagged it and
--     the resolver's honest over-selection label (tuple_pinned / pair_coarse /
--     similarity_narrowed / similarity_unnarrowed).
--   candidate_request_ids — the set the operator REVIEWED (the resolver output).
--   revoked_request_ids   — the subset the operator CHOSE to revoke. Both are
--     TEXT[]: an audit snapshot captured at decision time, read whole, never
--     independently queried — the per-request truth lives on the claim row
--     (pool_royalty_mints.status), so no child table / FK is warranted; the
--     evidence hashes (answer_sha256/prompt_sha256, migration 0045) stay on the
--     claim row and are joined by request_id, not copied here.
--   decided_by — AuthContext.UserID, or 'global_key' when empty (mirrors the
--     ApproveRate precedent). KNOWN, LOGGED limitation: admin identity is
--     global-key-only today; per-person attribution is pre-flip audit hardening.
--     decided_by is TEXT so tightening it later is a value change, not a schema
--     change.
--   outcome — the Revoker's RevokeReport (per-request_id outcomes + totals),
--     JSONB, NULLABLE: NULL between the INSERT (record-before-burn) and the
--     post-revoke UPDATE that completes it. A crash in that window leaves the
--     decision on disk with outcome NULL; the claim rows (status='revoked')
--     remain the authoritative money truth and the record reconciles against
--     them.
--
-- Additive, idempotent, own-file. No index: an audit table read by id /
-- decided_at, not a hot path; add one later if it grows.

CREATE TABLE IF NOT EXISTS pool_royalty_adjudications (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    flag_type             TEXT NOT NULL,            -- volume | self_dealing | similarity
    resolution_label      TEXT NOT NULL,            -- tuple_pinned | pair_coarse | similarity_narrowed | similarity_unnarrowed
    candidate_request_ids TEXT[] NOT NULL,          -- the set the operator reviewed
    revoked_request_ids   TEXT[] NOT NULL,          -- the subset the operator chose to revoke
    decided_by            TEXT NOT NULL,            -- AuthContext.UserID or 'global_key'
    outcome               JSONB,                    -- the RevokeReport; NULL until the revoke completes
    decided_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
