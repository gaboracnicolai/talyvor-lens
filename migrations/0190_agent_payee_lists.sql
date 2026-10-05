-- B28.303 — an agent's rules name who it may pay and who it may not.
--
-- allowed_payees and blocked_payees hold payee ids — an agent (agt_…), a marketplace listing (lst_…), a company
-- (its workspace id) or a card merchant (its network id). They are judged inside the payment's movement under the
-- payer's row lock, with its other rules: a payee on blocked_payees is refused, and once allowed_payees names any,
-- a payee it does not name is refused. Empty is no rule.
ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS allowed_payees TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS blocked_payees TEXT[] NOT NULL DEFAULT '{}';
