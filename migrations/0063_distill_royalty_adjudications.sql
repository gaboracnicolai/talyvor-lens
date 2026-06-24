-- 0063_distill_royalty_adjudications.sql — the distill reuse-royalty ADJUDICATION
-- audit log (PR3 of the distill anti-gaming arc). Mirrors 0048_pool_royalty_adjudications
-- exactly, but for distill_royalty_mints: the durable record-before-revoke row that
-- BINDS a deliberate operator decision to a held-mint claw-back, so no distill revoke
-- can ever happen without a preceding audit row. Written by the parameterized
-- AdjudicationWriter (NewAdjudicationWriterForTable(..., "distill_royalty_adjudications")).
-- Inert in the current config: an admin endpoint AND held distill rows are both
-- required, and held rows exist only under LENS_POOL_ROYALTY_MINTING_ENABLED.

CREATE TABLE IF NOT EXISTS distill_royalty_adjudications (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    flag_type             TEXT NOT NULL,            -- volume_swarm | self_dealing (distill detectors; no similarity)
    resolution_label      TEXT NOT NULL,            -- the resolver's honest over-selection label
    candidate_request_ids TEXT[] NOT NULL,          -- the set the operator reviewed
    revoked_request_ids   TEXT[] NOT NULL,          -- the subset the operator chose to revoke
    decided_by            TEXT NOT NULL,            -- AuthContext.UserID or 'global_key'
    outcome               JSONB,                    -- the RevokeReport; NULL until the revoke completes
    decided_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
