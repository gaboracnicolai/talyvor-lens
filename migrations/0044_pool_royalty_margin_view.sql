-- 0044_pool_royalty_margin_view.sql — Phase-2 Stage 2.2(a): the Pool-B margin
-- read surface (COORDINATION "Pool-B economics — DECIDED": fee-shaped,
-- MARGIN-IDENTITY ONLY).
--
-- Talyvor's realized (1−s) margin on a pooled hit is an IDENTITY over columns
-- the Stage-2.1 claim row already carries, committed atomically with every
-- mint:
--
--     margin_usd = avoided_cogs_usd − minted_amount
--
-- This view DERIVES it; nothing re-records it. Deliberately NOT a
-- token_events write: every customer spend reader (budgets, ROI, costanomaly,
-- forecast, anomaly, alerts.windowSpend, workspace SpendLimitUSD enforcement,
-- tenant spend, MCP/API summaries, audit export) sums token_events.cost_usd
-- with no row-type filter — a margin row there would be miscounted as
-- CUSTOMER spend and could push real customers toward their own spend caps.
-- cost_usd means COST; margin is REVENUE. The margin surface stays separate.
--
-- Rows exist only for genuinely minted royalties (the 2.1 claim + credit are
-- one transaction; AlreadyMinted retries, self-hits, and zero-COGS hits write
-- nothing), so SUM(margin_usd) over this view is exactly the realized margin.
-- With minting off (the default) the underlying table is empty and the view
-- returns no rows — the inert posture is unchanged.
--
-- Additive + idempotent: CREATE OR REPLACE, own file, touches no existing
-- table or query.

CREATE OR REPLACE VIEW pool_royalty_margin AS
SELECT
    request_id,
    requester_workspace_id,
    contributor_workspace_id,
    layer,
    provider,
    model,
    avoided_cogs_usd,
    minted_amount,
    avoided_cogs_usd - minted_amount AS margin_usd,
    created_at
FROM pool_royalty_mints;
