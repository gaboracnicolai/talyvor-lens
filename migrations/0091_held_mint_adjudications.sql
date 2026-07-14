-- 0091_held_mint_adjudications.sql — Phase-3 Item 2: the shared ADJUDICATION
-- record for the held tables Phase-2 left without a clawback surface
-- (eval_contribution_mints, routing_prediction_mints, node_latency_mints,
-- confidential_compute_mints, and traffic_mint_holds — the cache/compute/embedding
-- node mints).
--
-- Phase-2 gave pool_royalty_mints (0048) and distill_royalty_mints (0063) each a
-- purpose-built adjudications table because they carry a ring-shaped, two-workspace
-- decision. The five tables here are SINGLE-PARTY (a node/workspace earns for its
-- own contribution) or composite-keyed (traffic) — no ring, only a MANUAL operator
-- revoke. Their decision record is identical in shape, so they share ONE audit
-- table rather than five near-duplicates. Same columns as 0048 so the proven
-- AdjudicationWriterForTable writes it unchanged; the TARGET mint table is encoded
-- in flag_type (e.g. "manual:eval_contribution_mints"). The per-request money
-- truth still lives on each mint table's own `status` column (held|final|revoked).
-- The row is written BEFORE the burn (record-before-revoke), so a production revoke
-- can never happen without a preceding audit record — the 0048 durability guarantee.
--
-- For traffic_mint_holds the composite key (request_id, workspace_id, mint_type) is
-- encoded into candidate/revoked ids as "request_id|workspace_id|mint_type".
--
-- Additive, idempotent, own-file. Migration high-water 0090 → 0091 (Phase-2 added
-- no migration; no collision).

CREATE TABLE IF NOT EXISTS held_mint_adjudications (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    flag_type             TEXT NOT NULL,            -- "manual:<mint_table>" (the basis + target table)
    resolution_label      TEXT NOT NULL,            -- the operator's selection label
    candidate_request_ids TEXT[] NOT NULL,          -- the set the operator reviewed
    revoked_request_ids   TEXT[] NOT NULL,          -- the subset the operator chose to revoke
    decided_by            TEXT NOT NULL,            -- AuthContext.UserID or 'global_key'
    outcome               JSONB,                    -- the RevokeReport; NULL until the revoke completes
    decided_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
