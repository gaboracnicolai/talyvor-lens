-- B19.7 — one switch pauses every agent.
--
-- While a workspace has a row here, every hold, debit and payment of every one of its agents is refused
-- before the provider is called. Resuming deletes the row; an agent paused on its own (B19.6,
-- agent_accounts.paused_at) stays paused until it is resumed on its own.
CREATE TABLE IF NOT EXISTS agent_workspace_pauses (
    workspace_id TEXT PRIMARY KEY,
    reason       TEXT NOT NULL DEFAULT '',
    paused_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
