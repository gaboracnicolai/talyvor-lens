-- 0061_distill_royalty_basis.sql — L2/S4 PR2: the avoided-COGS BASIS for the
-- (parked, default-off) distill reuse-royalty. A descriptive money FIGURE only —
-- NO ledger, NO mint, NO caps/holdback/status/request_id-claim. Records, ONCE per
-- cross-tenant reuse RELATIONSHIP, the cost requester B avoided by being served
-- contributor A's cached OCR transcription instead of re-dispatching the vision
-- model.
--
-- WHY A SEPARATE TABLE (not a column on distill_serve_attribution, mig 0052):
-- 0052 is deliberately money-free (serve_count only, a per-serve counter). The
-- basis has a DIFFERENT cadence — captured ONCE per (owner, requester, content_hash)
-- relationship and never re-written — so it lives on its own row keyed by exactly
-- that relationship. The PK IS the once-per-relationship idempotency key the PR3
-- mint dedups on; PR3 mints off THIS deduplicated table, never the per-serve fact
-- stream, so once-per-relationship holds by construction across both tables.
--
-- THE NUMBER: avoided_cogs_usd = alerts.CostUSD(vision_model, vision_input_tokens,
-- vision_output_tokens) — A's ACTUAL vision-OCR cost (from the cached entry's
-- preserved basis, internal/distill.CachedOCR), snapshotted AT SERVE TIME (the OCR
-- cache is TTL-ephemeral Redis + model-keyed, so a later join cannot recover it).
-- The provenance columns (vision_model + token split) make avoided_cogs_usd
-- RE-DERIVABLE/auditable: an auditor (and PR3) can recompute CostUSD(stored model,
-- stored in, stored out) and confirm it equals the stored figure.
--
-- CAPTURE-ONCE / PINNED: written via INSERT ... ON CONFLICT (PK) DO NOTHING — a
-- re-serve of the same relationship NEVER overwrites the first-pinned basis, even
-- if the cached model/cost later changes, so PR3's mint amount per relationship is
-- stable (not upsert-latest).
--
-- A row is written ONLY when the three-switch distill-pooling consent already
-- authorized the cross-tenant OCR serve (LENS_DISTILL_POOLABLE_ENABLED + owner
-- opt-in + requester opt-in), AND owner != requester (self-serve skipped), AND the
-- requester's logging policy is not None. Unpartitioned, like
-- distill_serve_attribution / pool_royalty_mints. DOUBLE PRECISION matches
-- pool_royalty_mints.avoided_cogs_usd (0043) — a COGS estimate, not a settled
-- balance.

CREATE TABLE IF NOT EXISTS distill_royalty_basis (
    owner_workspace_id     TEXT             NOT NULL,   -- contributor (A): the pooled OCR's owner stamp
    requester_workspace_id TEXT             NOT NULL,   -- reuser (B): the requesting workspace
    content_hash           TEXT             NOT NULL,   -- distill.ContentHash(raw document bytes)
    avoided_cogs_usd       DOUBLE PRECISION NOT NULL,   -- CostUSD(vision_model, in, out): what B avoided
    vision_model           TEXT             NOT NULL,   -- provenance: the model A's OCR actually used
    vision_input_tokens    INTEGER          NOT NULL,   -- provenance: A's OCR input tokens
    vision_output_tokens   INTEGER          NOT NULL,   -- provenance: A's OCR output tokens
    captured_at            TIMESTAMPTZ      NOT NULL DEFAULT NOW(),
    PRIMARY KEY (owner_workspace_id, requester_workspace_id, content_hash)  -- once-per-relationship
);

-- Read path for PR3 (mint off the deduplicated basis) + the materiality probe:
-- "what avoided-COGS has each owner's reused work accrued, recently".
CREATE INDEX IF NOT EXISTS idx_distill_royalty_basis_owner
    ON distill_royalty_basis (owner_workspace_id, captured_at DESC);
