-- B17.14 — a spending limit is kept as it was set, however small.
--
-- 0028 gave limit_usd and spent_usd NUMERIC(12,4), so a limit under $0.0001 (a limit below what one
-- request costs) was stored as 0 — and a zero limit is "no limit", so a hard_block budget refused
-- nothing. They become DOUBLE PRECISION, the type of token_events.cost_usd that spent_usd is summed
-- from. Existing values are carried over unchanged.

ALTER TABLE budgets ALTER COLUMN limit_usd TYPE DOUBLE PRECISION;
ALTER TABLE budgets ALTER COLUMN spent_usd TYPE DOUBLE PRECISION;
