-- 0103_royalty_margin_peg_fix.sql — make the royalty MARGIN views report real
-- dollars at the published LENS peg.
--
-- THE BUG (0044/0064, carried through 0082): margin_usd was computed as
--     avoided_cogs_usd − (minted_amount / 1e6)
-- which subtracts LENS from DOLLARS as if 1 LENS = $1. The peg is 1 LENS = $0.10
-- (economy.LXCUSDValue = 0.10 = 1 / economy.LENSPerUSD). Paired with the anchor
-- fix (poolroyalty/anchor.go now mints s × avoided_COGS × LENSPerUSD, so a $10
-- avoided at s=0.5 mints 50 LENS = 50,000,000 µLENS), the old view produced
-- 10 − 50 = −$40 of "margin". Both halves are wrong in the same direction.
--
-- THE FIX: convert the minted µLENS back to dollars at the peg before the
-- subtraction, so margin_usd is the true (1−s) × avoided_COGS:
--     margin_usd = avoided_cogs_usd − (minted_amount / 1e6) * 0.10
--                = avoided_cogs_usd − minted_LENS * $0.10/LENS
-- For the $10 / 50-LENS example: 10 − (50 × 0.10) = 10 − 5 = $5 = (1−0.5) × 10.
--
-- The 0.10 literal is the peg (economy.LXCUSDValue); SQL can't reference the Go
-- constant, so it is written inline and pinned by
-- TestRoyaltyMarginViews_ShipPegDollars_Integration, which reads the SHIPPED
-- view DDL from these migrations and asserts $5 on the canonical row.
--
-- DROP-then-CREATE (not OR REPLACE): a column list/type change can't go through
-- OR REPLACE, and DROP-first is replay-safe (nothing reads the view between
-- migrations). Column shapes are otherwise byte-identical to 0082. Additive,
-- own file, touches no table or row — only the two derived views.

DROP VIEW IF EXISTS pool_royalty_margin;
CREATE VIEW pool_royalty_margin AS
SELECT
    request_id,
    requester_workspace_id,
    contributor_workspace_id,
    layer,
    provider,
    model,
    avoided_cogs_usd,
    minted_amount,
    avoided_cogs_usd - (minted_amount::numeric / 1000000.0) * 0.10 AS margin_usd, -- × $0.10/LENS peg
    created_at
FROM pool_royalty_mints;

DROP VIEW IF EXISTS distill_royalty_margin;
CREATE VIEW distill_royalty_margin AS
SELECT
    request_id,
    requester_workspace_id,
    contributor_workspace_id,
    content_hash,
    avoided_cogs_usd,
    minted_amount,
    avoided_cogs_usd - (minted_amount::numeric / 1000000.0) * 0.10 AS margin_usd, -- × $0.10/LENS peg
    status,
    created_at
FROM distill_royalty_mints;
