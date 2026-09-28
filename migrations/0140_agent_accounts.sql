-- B19.1 — agent accounts: every agent has its own balance, on a double-entry ledger.
--
-- A workspace creates agents, gives each its own API keys (agent_account_keys, one agent per key),
-- funds them and takes funds back. An agent's balance is never stored: it is the sum of its
-- postings. Every movement is ONE entry of postings that sum to zero, across three kinds of account:
--
--   'workspace'     the workspace's own side — funding an agent moves LXC out of it, a withdrawal back
--   'agent:<id>'    the agent's balance
--   'spend'         what the agent spent on Talyvor's services (holds, settles and releases included)
--
-- The LXC itself stays in the workspace's lxc_balances, debited by the existing spend path exactly as
-- before; these postings say whose it is. So Σ agent balances ≤ the workspace's LXC balance, and the
-- 'spend' account equals the lxc_ledger movements of the agents' keys.
CREATE TABLE IF NOT EXISTS agent_accounts (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    name         TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_agent_accounts_workspace ON agent_accounts (workspace_id);

CREATE TABLE IF NOT EXISTS agent_account_keys (
    scoped_key_id TEXT PRIMARY KEY,                               -- the API key (auth APIKeyID)
    agent_id      TEXT NOT NULL REFERENCES agent_accounts (id) ON DELETE CASCADE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS agent_postings (
    id           BIGSERIAL PRIMARY KEY,
    entry_id     UUID NOT NULL,                                   -- the postings of one movement
    workspace_id TEXT NOT NULL,
    account      TEXT NOT NULL,                                   -- workspace | agent:<id> | spend
    amount_ulxc  BIGINT NOT NULL CHECK (amount_ulxc <> 0),
    kind         TEXT NOT NULL,                                   -- fund | withdraw | spend | hold | settle | release
    ref          TEXT NOT NULL DEFAULT '',                        -- request or reservation id
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_agent_postings_account ON agent_postings (workspace_id, account);
CREATE INDEX IF NOT EXISTS idx_agent_postings_entry ON agent_postings (entry_id);

-- Append-only, like every other money record (0055).
CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON agent_postings
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON agent_postings
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();

-- Double entry: at commit, every entry a transaction wrote must sum to zero, or the commit fails.
CREATE OR REPLACE FUNCTION agent_postings_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (SELECT sum(amount_ulxc) FROM agent_postings WHERE entry_id = NEW.entry_id) <> 0 THEN
        RAISE EXCEPTION 'agent_postings entry % does not balance', NEW.entry_id;
    END IF;
    RETURN NULL;
END;
$$;
DROP TRIGGER IF EXISTS agent_postings_balanced ON agent_postings;
CREATE CONSTRAINT TRIGGER agent_postings_balanced AFTER INSERT ON agent_postings
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION agent_postings_balanced();
