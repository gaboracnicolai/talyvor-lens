-- 0064_distill_royalty_margin_view.sql — the distill reuse-royalty MARGIN read
-- surface (PR4 of the distill anti-gaming arc). Mirrors 0044_pool_royalty_margin_view
-- for distill_royalty_mints. Talyvor's realized (1−s) margin on a distill OCR reuse is
-- the same identity over columns the mint claim row already carries:
--     margin_usd = avoided_cogs_usd − minted_amount
-- This view DERIVES it; nothing re-records it. Deliberately NOT a token_events write —
-- every customer-spend reader sums token_events.cost_usd with no row-type filter, so a
-- margin row there would be miscounted as CUSTOMER spend. cost_usd means COST; margin is
-- REVENUE. With minting off (the default) the underlying table is empty and the view
-- returns no rows — the inert posture is unchanged. Additive; touches no existing object.
--
-- Distill has NO layer/provider/model columns (it is exact-content OCR reuse), so this
-- view carries content_hash in their place. DROP-then-CREATE for replay-safety (mirrors 0044).

DROP VIEW IF EXISTS distill_royalty_margin;
CREATE VIEW distill_royalty_margin AS
SELECT
    request_id,
    requester_workspace_id,
    contributor_workspace_id,
    content_hash,
    avoided_cogs_usd,
    minted_amount,
    avoided_cogs_usd - minted_amount AS margin_usd,
    status,
    created_at
FROM distill_royalty_mints;
