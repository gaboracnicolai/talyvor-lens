-- B19.9 — agents use the bank themselves, through the MCP server, with their own keys.
--
-- An agent asks for a payment's approval with a reason (agent_approvals.reason), and every call it makes
-- to the bank's tools is logged here, append-only like every other money record (0055): which key, which
-- agent, which tool, the arguments, and whether it was served, refused or failed.
ALTER TABLE agent_approvals ADD COLUMN IF NOT EXISTS reason TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS agent_tool_calls (
    id            BIGSERIAL PRIMARY KEY,
    workspace_id  TEXT NOT NULL,
    agent_id      TEXT NOT NULL,            -- '' when the key is attached to no agent
    scoped_key_id TEXT NOT NULL,
    tool          TEXT NOT NULL,
    arguments     JSONB NOT NULL DEFAULT '{}',
    outcome       TEXT NOT NULL CHECK (outcome IN ('ok', 'refused', 'error')),
    detail        TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_agent_tool_calls_agent ON agent_tool_calls (workspace_id, agent_id, created_at DESC);

CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON agent_tool_calls
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON agent_tool_calls
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();
