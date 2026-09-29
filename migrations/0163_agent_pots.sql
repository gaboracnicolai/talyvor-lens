-- B22.7 — pots: an agent keeps money aside for a goal.
--
-- An agent splits its balance into named pots — a goal (with a target), a budget or a reserve — and moves
-- credits between a pot and its main balance: an agent_postings entry agent:<id> ↔ pot:<pot id> in the
-- agent's workspace. A pot's credits are still the agent's, counted with what its agents hold, but the agent
-- spends only its main balance. A pot can be locked until a date: moving credits out before then is refused.
-- No interest (interest is RED). GREEN (rules_approvals_statements_pots). Statements show each pot.
CREATE TABLE IF NOT EXISTS agent_pots (
    id           TEXT PRIMARY KEY,                                   -- pot_<uuid>
    workspace_id TEXT NOT NULL,
    agent_id     TEXT NOT NULL,
    name         TEXT NOT NULL,
    kind         TEXT NOT NULL CHECK (kind IN ('goal', 'budget', 'reserve')),
    target_ulxc  BIGINT NOT NULL DEFAULT 0 CHECK (target_ulxc >= 0),  -- a goal's target; 0 for none
    locked_until TIMESTAMPTZ,                                        -- nothing moves out before this
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (agent_id, name)
);
CREATE INDEX IF NOT EXISTS idx_agent_pots_agent ON agent_pots (workspace_id, agent_id);
