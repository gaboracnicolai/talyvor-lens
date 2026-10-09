-- B30.14 — account details others can pay into.
--
-- An agent's account has no bank details of its own: a payer pays its company's account details and quotes the
-- agent account's payment reference, which routes the money to it. The reference is TLV and twelve hex digits drawn
-- from the account's id — fifteen characters, inside the eighteen a UK Faster Payments reference carries — and is
-- one only among its company account's agent accounts, which is where a payment in looks for it.

ALTER TABLE money_accounts ADD COLUMN IF NOT EXISTS payment_reference TEXT
    GENERATED ALWAYS AS (CASE WHEN purpose = 'agent' THEN 'TLV' || upper(substr(md5(id), 1, 12)) END) STORED;

CREATE UNIQUE INDEX IF NOT EXISTS uq_money_accounts_payment_reference
    ON money_accounts (parent_account_id, payment_reference) WHERE payment_reference IS NOT NULL;
