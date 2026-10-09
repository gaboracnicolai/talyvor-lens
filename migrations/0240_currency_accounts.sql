-- B30.13 — accounts in pounds, euros and dollars for a company and each of its agents.
--
-- A company has at most one open account in each currency, opened at the account partner; an agent's account is a
-- sub-account of its company's in the same currency, and parent_account_id names that company account. An agent has
-- at most one open account in each currency. A closed account leaves room for a new one.

ALTER TABLE money_accounts ADD COLUMN IF NOT EXISTS parent_account_id TEXT REFERENCES money_accounts (id);

CREATE UNIQUE INDEX IF NOT EXISTS uq_money_accounts_company_currency
    ON money_accounts (workspace_id, currency) WHERE purpose = 'company' AND status <> 'closed';
CREATE UNIQUE INDEX IF NOT EXISTS uq_money_accounts_agent_currency
    ON money_accounts (agent_id, currency) WHERE purpose = 'agent' AND status <> 'closed';
