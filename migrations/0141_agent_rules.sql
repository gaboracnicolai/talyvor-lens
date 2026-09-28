-- B19.2 — spending rules per agent, enforced before the provider is called.
--
-- One row of rules per agent (agent_rules): limits per request, per day and per month; the models and
-- providers it may use; an amount above which a person must approve the request; and the hours it may
-- spend in. A NULL limit, an empty list or a NULL window is no rule. The rules are enforced in the same
-- transaction as the agent's hold or debit, under the agent's row lock, so concurrent requests cannot
-- together pass a daily or monthly limit.
--
-- A request above the approval amount files an agent_approvals row and is refused; once the workspace's
-- owner approves it, the same request (by fingerprint: the key, the model and the prompt, hashed) is let
-- through exactly once.
CREATE TABLE IF NOT EXISTS agent_rules (
    agent_id             TEXT PRIMARY KEY REFERENCES agent_accounts (id) ON DELETE CASCADE,
    workspace_id         TEXT NOT NULL,
    max_per_request_ulxc BIGINT CHECK (max_per_request_ulxc > 0),
    daily_limit_ulxc     BIGINT CHECK (daily_limit_ulxc > 0),
    monthly_limit_ulxc   BIGINT CHECK (monthly_limit_ulxc > 0),
    approval_above_ulxc  BIGINT CHECK (approval_above_ulxc > 0),
    allowed_models       TEXT[] NOT NULL DEFAULT '{}',
    allowed_providers    TEXT[] NOT NULL DEFAULT '{}',
    active_from          TEXT,                                     -- 'HH:MM' in timezone
    active_until         TEXT,
    timezone             TEXT NOT NULL DEFAULT 'UTC',
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((active_from IS NULL) = (active_until IS NULL))
);
CREATE INDEX IF NOT EXISTS idx_agent_rules_workspace ON agent_rules (workspace_id);

CREATE TABLE IF NOT EXISTS agent_approvals (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    agent_id     TEXT NOT NULL REFERENCES agent_accounts (id) ON DELETE CASCADE,
    fingerprint  TEXT NOT NULL,
    amount_ulxc  BIGINT NOT NULL,
    model        TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'denied', 'used')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at   TIMESTAMPTZ,
    used_ref     TEXT
);
-- One open approval per request: a retry before the decision finds the same row.
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_approvals_open ON agent_approvals (agent_id, fingerprint)
    WHERE status IN ('pending', 'approved');
CREATE INDEX IF NOT EXISTS idx_agent_approvals_workspace ON agent_approvals (workspace_id, status);
