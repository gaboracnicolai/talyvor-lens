-- B28.297 — every agent-ledger account keeps a stored running balance, so reading a balance is one row.
--
-- Until now a balance was the sum of the account's postings (0140), re-summed on every money move: a cost
-- that grows with the account's whole history. agent_account_balances holds that sum. A trigger on
-- agent_postings adds each posting to it in the transaction that writes the posting, so the two cannot
-- disagree, and every writer — fund, withdraw, hold, settle, release, pay, transfer, pot, escrow, card,
-- cash-out, reversal — keeps it without being touched. The postings stay the record; this is their total.
--
-- 'workspace', 'spend' and 'cashed_out' are not kept: each is one account per workspace that all of its agents
-- post to, and nothing reads it as a balance, so storing it would make every agent of a workspace wait on
-- one row.
CREATE TABLE IF NOT EXISTS agent_account_balances (
    workspace_id TEXT NOT NULL,
    account      TEXT NOT NULL,                                   -- agent:<id> | pot:<id> | escrow:<id> | …
    balance_ulxc BIGINT NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, account)
);

CREATE OR REPLACE FUNCTION agent_account_balances_post() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO agent_account_balances (workspace_id, account, balance_ulxc)
    VALUES (NEW.workspace_id, NEW.account, NEW.amount_ulxc)
    ON CONFLICT (workspace_id, account) DO UPDATE
        SET balance_ulxc = agent_account_balances.balance_ulxc + EXCLUDED.balance_ulxc, updated_at = now();
    RETURN NULL;
END;
$$;

-- No posting lands between the trigger and the backfill, so together they count every posting exactly once.
LOCK TABLE agent_postings IN SHARE ROW EXCLUSIVE MODE;
DROP TRIGGER IF EXISTS agent_account_balances_post ON agent_postings;
CREATE TRIGGER agent_account_balances_post AFTER INSERT ON agent_postings
    FOR EACH ROW WHEN (NEW.account NOT IN ('workspace', 'spend', 'cashed_out'))
    EXECUTE FUNCTION agent_account_balances_post();
INSERT INTO agent_account_balances (workspace_id, account, balance_ulxc)
SELECT workspace_id, account, sum(amount_ulxc) FROM agent_postings
 WHERE account NOT IN ('workspace', 'spend', 'cashed_out')
 GROUP BY workspace_id, account
ON CONFLICT (workspace_id, account) DO UPDATE SET balance_ulxc = EXCLUDED.balance_ulxc, updated_at = now();
