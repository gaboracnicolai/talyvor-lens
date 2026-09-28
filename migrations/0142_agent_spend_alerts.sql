-- B19.6 — unusual spend is flagged, and an agent can be paused.
--
-- An agent is paused while paused_at is set: every hold, debit and payment it makes is refused before
-- the provider is called, until the workspace's owner resumes it. Its owner pauses it by hand, or its
-- rules (pause_on_unusual_spend) pause it when its spend runs well above its own pattern.
--
-- The rule (internal/economy/agent_alerts.go): an alert is raised when what the agent has spent in the
-- last hour, with the movement being judged, reaches five times its usual hourly rate — what it spent in
-- the seven days before that hour, over 168. An agent with nothing spent in those seven days has no
-- pattern yet and raises none; an agent raises at most one alert an hour.
ALTER TABLE agent_accounts ADD COLUMN IF NOT EXISTS paused_at TIMESTAMPTZ;
ALTER TABLE agent_accounts ADD COLUMN IF NOT EXISTS paused_reason TEXT NOT NULL DEFAULT '';

ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS pause_on_unusual_spend BOOLEAN NOT NULL DEFAULT false;

CREATE TABLE IF NOT EXISTS agent_spend_alerts (
    id                    TEXT PRIMARY KEY,
    workspace_id          TEXT NOT NULL,
    agent_id              TEXT NOT NULL REFERENCES agent_accounts (id) ON DELETE CASCADE,
    last_hour_ulxc        BIGINT NOT NULL,   -- spent in the hour, with the movement that raised it
    usual_per_hour_ulxc   BIGINT NOT NULL,   -- the seven days before that hour, over 168
    paused                BOOLEAN NOT NULL,  -- the agent was paused by it
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_agent_spend_alerts_workspace ON agent_spend_alerts (workspace_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_agent_spend_alerts_agent ON agent_spend_alerts (agent_id, created_at DESC);
