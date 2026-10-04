-- 0183_token_events_saving.sql — B27.32: the saving Talyvor shows is measured, one request at a time.
--
-- Each token_events row now records what the request would have cost and what it was charged, so a
-- workspace's saving is a SUM over its own rows rather than a percentage nobody computed:
--
--   requested_model  the model the request asked for ('' = not recorded)
--   list_cost_usd    what it would have cost at that model with no Talyvor cache in the path — priced on
--                    the provider-side cache breakdown the request actually had, because that discount is
--                    the customer's with or without Lens
--   charged_usd      what this request was charged
--
-- NULL list/charged = not measured: every row written before this migration, and the OCR sub-call rows a
-- distilled request books beside its own. The saving reader sums only measured rows and counts the rest.
--
-- Additive, own file, no row rewritten. ADD COLUMN on the partitioned parent reaches every partition.

ALTER TABLE token_events
    ADD COLUMN IF NOT EXISTS requested_model TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS list_cost_usd DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS charged_usd DOUBLE PRECISION;
