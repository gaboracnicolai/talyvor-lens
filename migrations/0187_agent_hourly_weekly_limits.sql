-- B28.300 — an agent's rules can cap what it spends in an hour and in a week.
--
-- Counted like the daily and monthly limits, in the agent's timezone, inside its hold or debit under its row
-- lock: the hour is the clock hour, the week starts on Monday. NULL is no limit.
ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS hourly_limit_ulxc BIGINT CHECK (hourly_limit_ulxc > 0);
ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS weekly_limit_ulxc BIGINT CHECK (weekly_limit_ulxc > 0);
