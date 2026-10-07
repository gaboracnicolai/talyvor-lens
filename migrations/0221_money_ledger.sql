-- B30.2 — money in currencies: accounts and a double-entry ledger in pounds, euros, dollars and USDC.
--
-- A money account holds one currency — an ISO 4217 code, or USDC — for a workspace, and for one of its agents
-- when agent_id is set. Its purpose says what it is: the company's or an agent's own money, a pot, a hold, a
-- suspense account for money not yet matched, what has come in or gone out through a partner, or Talyvor's fees.
--
-- Every movement is one entry of postings, in int64 minor units (pence, cents, and millionths of a USDC) with the
-- currency on every posting, never a float. A posting's amount is what it adds to its account: money in is
-- positive, money out negative. An entry whose postings do not sum to zero per currency and funding — or that has
-- no postings — is refused when its transaction commits; a posting is never changed or deleted; a posting's
-- currency is its account's; and each account's balance is kept in money_account_balances by a trigger, in the
-- transaction that writes the posting, as agent_account_balances is (0185). Test money never balances live money.
--
-- Credits (LXC) stay where they are, in lxc_ledger and agent_postings: this ledger does not touch them.

CREATE TABLE IF NOT EXISTS money_accounts (
    id           TEXT PRIMARY KEY,                                  -- macc_<uuid>
    workspace_id TEXT NOT NULL,
    agent_id     TEXT REFERENCES agent_accounts (id),
    currency     TEXT NOT NULL CHECK (currency ~ '^[A-Z]{3}$' OR currency = 'USDC'),
    purpose      TEXT NOT NULL CHECK (purpose IN ('company', 'agent', 'pot', 'hold', 'suspense', 'partner', 'revenue')),
    status       TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'frozen', 'closed')),
    name         TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (id, currency),                                          -- what pins a posting's currency to its account's
    CHECK (purpose <> 'agent' OR agent_id IS NOT NULL),
    CHECK (purpose NOT IN ('company', 'suspense', 'partner', 'revenue') OR agent_id IS NULL)
);
CREATE INDEX IF NOT EXISTS idx_money_accounts_workspace ON money_accounts (workspace_id, agent_id);

CREATE TABLE IF NOT EXISTS money_entries (
    id              TEXT PRIMARY KEY,                               -- mle_<uuid>
    workspace_id    TEXT NOT NULL,                                  -- whose movement it is
    capability      TEXT NOT NULL,                                  -- the wallet capability it moved for (B30.1)
    kind            TEXT NOT NULL CHECK (kind ~ '^[a-z][a-z_]*$'),
    idempotency_key TEXT NOT NULL CHECK (idempotency_key <> ''),
    memo            TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, idempotency_key)                          -- a retried movement is the same entry
);

CREATE TABLE IF NOT EXISTS money_postings (
    entry_id     TEXT NOT NULL REFERENCES money_entries (id),
    line         SMALLINT NOT NULL CHECK (line > 0),
    account_id   TEXT NOT NULL,
    amount_minor BIGINT NOT NULL CHECK (amount_minor <> 0),
    currency     TEXT NOT NULL,
    funding      TEXT NOT NULL CHECK (funding IN ('test', 'live')),
    PRIMARY KEY (entry_id, line),
    FOREIGN KEY (account_id, currency) REFERENCES money_accounts (id, currency)
);
CREATE INDEX IF NOT EXISTS idx_money_postings_account ON money_postings (account_id);

-- An entry balances, per currency and funding, and has postings, or its transaction does not commit.
CREATE OR REPLACE FUNCTION money_entry_balanced() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    entry TEXT;
    off   RECORD;
BEGIN
    IF TG_TABLE_NAME = 'money_entries' THEN
        entry := NEW.id;
    ELSE
        entry := NEW.entry_id;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM money_postings WHERE entry_id = entry) THEN
        RAISE EXCEPTION 'money entry % has no postings', entry USING ERRCODE = 'check_violation';
    END IF;
    SELECT currency, funding, sum(amount_minor) AS total INTO off FROM money_postings
     WHERE entry_id = entry GROUP BY currency, funding HAVING sum(amount_minor) <> 0 LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'money entry % does not balance: its % % postings sum to % minor units', entry, off.funding, off.currency, off.total
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END $$;
DROP TRIGGER IF EXISTS money_entry_balanced ON money_entries;
CREATE CONSTRAINT TRIGGER money_entry_balanced AFTER INSERT ON money_entries
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION money_entry_balanced();
DROP TRIGGER IF EXISTS money_postings_balanced ON money_postings;
CREATE CONSTRAINT TRIGGER money_postings_balanced AFTER INSERT ON money_postings
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION money_entry_balanced();

-- The ledger is append-only, like every other money record (0055).
CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON money_entries
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON money_entries
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON money_postings
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON money_postings
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();

-- Each account's total, per funding: the postings stay the record, this is their sum.
CREATE TABLE IF NOT EXISTS money_account_balances (
    account_id    TEXT NOT NULL,
    funding       TEXT NOT NULL,
    currency      TEXT NOT NULL,
    balance_minor BIGINT NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, funding)
);

CREATE OR REPLACE FUNCTION money_account_balances_post() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO money_account_balances (account_id, funding, currency, balance_minor)
    VALUES (NEW.account_id, NEW.funding, NEW.currency, NEW.amount_minor)
    ON CONFLICT (account_id, funding) DO UPDATE
        SET balance_minor = money_account_balances.balance_minor + EXCLUDED.balance_minor, updated_at = now();
    RETURN NULL;
END $$;
DROP TRIGGER IF EXISTS money_account_balances_post ON money_postings;
CREATE TRIGGER money_account_balances_post AFTER INSERT ON money_postings
    FOR EACH ROW EXECUTE FUNCTION money_account_balances_post();
