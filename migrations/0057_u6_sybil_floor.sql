-- 0057_u6_sybil_floor.sql
-- U6 Sybil floor (PR 1 of 2): two additive, default-safe primitives. The gate
-- before the token economy can be flipped public — nothing here turns minting on.
--
-- 1) workspaces.earn_verified — the verified-to-earn admin-vouch OVERRIDE.
--    A workspace may MINT / accrue royalty only when verified-to-earn:
--      workspaceMayEarn(wsID) ⟺ earn_verified = true
--                              OR EXISTS(completed lxc_purchase for wsID).
--    The completed-purchase half is derived at READ time (no write to the money
--    path, no race); this column is the enterprise/self-host vouch override.
--    Default false = the safe state. No backfill: pre-existing workspaces are
--    unverified until they purchase real LXC or an admin vouches. Pure column
--    add — re-runnable.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS earn_verified BOOLEAN NOT NULL DEFAULT false;

-- 2) mint_idempotency — the (request_id, workspace_id, mint_type) claim row that
--    makes the previously-unprotected mint tracks (compute / cache / embedding)
--    replay-safe, generalizing the pattern track's (request_id, workspace_id)
--    composite to a shared table. mint_type is in the key so one track's claim
--    can never suppress another's. request_id MUST be SERVER-derived work-product
--    content (a caller-supplied id defeats idempotency); an empty request_id
--    mints nothing (fail-closed, enforced in CreditOnce). A colliding claim can
--    only SUPPRESS a mint (deflationary), never inflate.
CREATE TABLE IF NOT EXISTS mint_idempotency (
    request_id   TEXT NOT NULL,
    workspace_id TEXT NOT NULL,
    mint_type    TEXT NOT NULL,
    amount       DOUBLE PRECISION NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (request_id, workspace_id, mint_type)
);
