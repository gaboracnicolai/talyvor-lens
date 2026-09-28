-- B19.12 — agent cards in test mode, through Stripe Issuing.
--
-- Each agent may hold one virtual card (agent_cards). Every purchase with it reaches Lens as a Stripe
-- issuing_authorization.request and is approved or declined there and then by the agent's spending rules
-- (B19.2) and its balance. EVERY authorisation request — approved or declined, a card Lens does not know
-- included — is one row of agent_card_authorizations, append-only. An approved one also debits the agent:
-- one lxc_ledger row (type 'agent_card') and one agent_postings entry (kind 'card', agent → spend), in the
-- same transaction as its row.
--
-- The card's currency is the platform's (GBP for a UK platform); the agent's balance is LXC at the USD peg.
-- Each authorisation is converted at the day's European Central Bank reference rate — the latest one
-- published on or before its date — and the rate is written on its row: the ECB publishes each currency per
-- EUR, so the USD value is amount × (USD per EUR) ÷ (currency per EUR). Cards are class RED (B22): test mode
-- only, so a card or an authorisation in live mode is refused.
CREATE TABLE IF NOT EXISTS agent_cards (
    id                   TEXT PRIMARY KEY,                             -- Stripe's card id (ic_…)
    workspace_id         TEXT NOT NULL,
    agent_id             TEXT NOT NULL UNIQUE REFERENCES agent_accounts (id) ON DELETE CASCADE,
    stripe_cardholder_id TEXT NOT NULL,
    last4                TEXT NOT NULL,
    exp_month            INT NOT NULL,
    exp_year             INT NOT NULL,
    currency             TEXT NOT NULL,
    livemode             BOOLEAN NOT NULL DEFAULT false CHECK (NOT livemode),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_agent_cards_workspace ON agent_cards (workspace_id);

CREATE TABLE IF NOT EXISTS agent_card_authorizations (
    id                    TEXT PRIMARY KEY,                            -- the Stripe event (evt_…): one per request
    authorization_id      TEXT NOT NULL,                               -- iauth_…; an increment is a second request
    card_id               TEXT NOT NULL,
    workspace_id          TEXT NOT NULL DEFAULT '',                    -- '' for a card no agent holds
    agent_id              TEXT NOT NULL DEFAULT '',
    approved              BOOLEAN NOT NULL,
    reason                TEXT NOT NULL,                               -- why, in words
    approval_id           TEXT NOT NULL DEFAULT '',                    -- the approval a person must give, if one was filed
    amount_minor          BIGINT NOT NULL,                             -- in the card's currency, smallest unit
    currency              TEXT NOT NULL,
    merchant_amount_minor BIGINT NOT NULL DEFAULT 0,
    merchant_currency     TEXT NOT NULL DEFAULT '',
    merchant_name         TEXT NOT NULL DEFAULT '',
    merchant_category     TEXT NOT NULL DEFAULT '',
    rate_date             DATE,                                        -- the ECB reference rate used; NULL for USD
    ecb_usd_per_eur       NUMERIC,
    ecb_currency_per_eur  NUMERIC,
    amount_usd_micros     BIGINT,                                      -- NULL when it could not be priced
    amount_ulxc           BIGINT,                                      -- what it cost; debited only when approved
    livemode              BOOLEAN NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_agent_card_authorizations_agent ON agent_card_authorizations (agent_id, created_at DESC);

CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON agent_card_authorizations
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON agent_card_authorizations
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();

-- The ECB's euro foreign exchange reference rates, as published (each currency per 1 EUR).
CREATE TABLE IF NOT EXISTS ecb_reference_rates (
    rate_date  DATE NOT NULL,
    currency   TEXT NOT NULL,
    per_eur    NUMERIC NOT NULL CHECK (per_eur > 0),
    fetched_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (rate_date, currency)
);
