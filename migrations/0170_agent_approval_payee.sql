-- 0170_agent_approval_payee.sql — B23.5: an approval says who is being paid, and why.
--
-- An approval a payment files names its payee — an agent, a marketplace listing, a company or a card
-- merchant — by id and by the name it had when the approval was filed, and carries the payment's memo,
-- so the person approving sees who the money goes to. A request to a model files none of these.

ALTER TABLE agent_approvals
    ADD COLUMN IF NOT EXISTS payee_kind TEXT NOT NULL DEFAULT ''
        CHECK (payee_kind IN ('', 'agent', 'listing', 'company', 'merchant')),
    ADD COLUMN IF NOT EXISTS payee_id   TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS payee_name TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS memo       TEXT NOT NULL DEFAULT '';
