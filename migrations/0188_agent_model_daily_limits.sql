-- B28.301 — an agent's rules can cap what it spends on one model in a day.
--
-- model_daily_limits_ulxc maps a model, named as the agent asks for it, to its daily cap: counted in the agent's
-- timezone inside its hold or debit under its row lock, like the daily limit. What it counts is the agent's
-- postings that name the model — a question's hold, debit, settle and release do from now on; postings written
-- before this, and payments, name none.
ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS model_daily_limits_ulxc JSONB NOT NULL DEFAULT '{}'::jsonb
    CHECK (jsonb_typeof(model_daily_limits_ulxc) = 'object');
ALTER TABLE agent_postings ADD COLUMN IF NOT EXISTS model TEXT;
