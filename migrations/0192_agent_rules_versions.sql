-- B28.307 — every change to an agent's rules is a version, and the rules can be rolled back to any of them.
--
-- A version is the whole agent_rules row as the change left it (to_jsonb, less agent_id, workspace_id and
-- updated_at), numbered from 1 per agent, with who changed it and how: "set", "template <id>" or "rollback to <n>".
-- Rolling back writes the version's row back over agent_rules — every column it names, so the rules are exactly as
-- they were — and is itself a new version. A rule added after a version was taken is not in it, and a rollback
-- keeps that rule as it is.
CREATE TABLE IF NOT EXISTS agent_rules_versions (
    agent_id     TEXT NOT NULL REFERENCES agent_accounts (id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL,
    version      INTEGER NOT NULL CHECK (version > 0),
    rules        JSONB NOT NULL CHECK (jsonb_typeof(rules) = 'object'),
    changed_by   TEXT NOT NULL DEFAULT '',                 -- the credential that changed them; empty when unknown
    change       TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_id, version)
);
CREATE INDEX IF NOT EXISTS idx_agent_rules_versions_workspace ON agent_rules_versions (workspace_id);

-- Rules set before this are each agent's version 1, so there is an earlier version to roll back to.
INSERT INTO agent_rules_versions (agent_id, workspace_id, version, rules, change, created_at)
SELECT agent_id, workspace_id, 1, to_jsonb(r) - 'agent_id' - 'workspace_id' - 'updated_at', 'before history', updated_at
  FROM agent_rules r
ON CONFLICT DO NOTHING;
