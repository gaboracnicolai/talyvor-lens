-- B28.308 — a temporary limit boost that reverts itself.
--
-- A boost raises one of an agent's limits to value until a time. The rules judge it at the request's time: before
-- until, the limit is the higher of the rule and the boost; from until on, the boost is not read and the rule is as
-- it was. Nothing has to run at expiry to revert it, and agent_rules — and so its versions (B28.307) — is never
-- touched. One boost per agent and limit: a new one replaces the last.
CREATE TABLE IF NOT EXISTS agent_rule_boosts (
    agent_id     TEXT NOT NULL REFERENCES agent_accounts (id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL,
    rule         TEXT NOT NULL CHECK (rule IN ('max_per_request_ulxc', 'hourly_limit_ulxc', 'daily_limit_ulxc',
                     'weekly_limit_ulxc', 'monthly_limit_ulxc', 'approval_above_ulxc', 'requests_per_minute')),
    value        BIGINT NOT NULL CHECK (value > 0),
    until        TIMESTAMPTZ NOT NULL,
    created_by   TEXT NOT NULL DEFAULT '',                 -- the credential that set it; empty when unknown
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_id, rule)
);
CREATE INDEX IF NOT EXISTS idx_agent_rule_boosts_workspace ON agent_rule_boosts (workspace_id);
