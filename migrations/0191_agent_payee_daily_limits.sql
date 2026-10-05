-- B28.304 — an agent's rules can cap how much it pays one payee in a day.
--
-- payee_daily_limits_ulxc maps a payee's id — an agent, a listing, a company (its agents and listings too) or a card
-- merchant, as the payee lists name them (0190) — to its daily cap, counted in the agent's timezone inside the
-- payment's movement under the payer's row lock, like the daily limit.
--
-- What it counts is agent_payee_payments: one row for every payment an agent's rules let through, written in the
-- payment's own transaction, so it commits exactly when the payment does. A payment reaches its payee by many
-- ledgers — pay postings, transfers, escrows, card authorizations, marketplace uses, loans — and this is the one
-- place all of them name it. payee_ids are the ids a cap may name the payee by: its own, and for an agent or a
-- listing the company that has it. Payments before this have no row, so a cap counts from today's first payment
-- after it.
ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS payee_daily_limits_ulxc JSONB NOT NULL DEFAULT '{}'::jsonb
    CHECK (jsonb_typeof(payee_daily_limits_ulxc) = 'object');

CREATE TABLE IF NOT EXISTS agent_payee_payments (
    id           BIGSERIAL PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    agent_id     TEXT NOT NULL,                       -- the payer
    payee_kind   TEXT NOT NULL,                       -- agent | listing | company | merchant
    payee_ids    TEXT[] NOT NULL,                     -- the payee's id, then its company's when it has one
    amount_ulxc  BIGINT NOT NULL CHECK (amount_ulxc > 0),
    ref          TEXT NOT NULL DEFAULT '',            -- the movement: its entry, reservation or use
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_agent_payee_payments_agent ON agent_payee_payments (agent_id, created_at);

-- Append-only, like every other money record (0055).
CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON agent_payee_payments
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON agent_payee_payments
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();
