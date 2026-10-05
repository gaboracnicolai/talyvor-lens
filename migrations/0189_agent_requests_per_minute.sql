-- B28.302 — an agent's owner can cap how many requests it makes a minute (until now only the operator could, per
-- workspace: rate_limit_rpm).
--
-- requests_per_minute is judged inside the agent's hold or debit under its row lock, like its spending limits, so
-- concurrent requests cannot together pass it: what it counts is the questions the agent held or debited in the
-- last sixty seconds — its 'hold' and 'spend' postings, one per question. NULL is no cap.
ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS requests_per_minute INTEGER CHECK (requests_per_minute > 0);
