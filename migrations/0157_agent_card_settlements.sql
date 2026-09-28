-- B19.25 — a card purchase's capture, reversal or refund settles the agent's balance.
--
-- An approved authorisation debits the agent the moment it is approved (0156): that debit is the
-- authorisation's HOLD. What becomes of it reaches Lens afterwards on the regular Stripe webhook —
-- issuing_authorization.updated and issuing_transaction.created — and each such event that settles anything
-- is one row here, append-only, in the same transaction as the ledger rows it moves (never an edit):
--
--   capture  a merchant took the money: it consumes the hold, and only a capture ABOVE the hold moves the
--            agent (debited the difference);
--   release  the authorisation closed, was reversed or expired — or was partly reversed while pending: what
--            is still held goes back to the agent. A hold released in full is credited exactly what it debited,
--            so a capture BELOW the hold is credited the difference here;
--   refund   the merchant gave money back: the agent is credited.
--
-- What an authorisation still holds is Σ its approved requests (0156) − Σ hold_minor / hold_ulxc here.
-- Amounts are converted at the ECB rate recorded on the authorisation's row. Test mode only, like the cards.
CREATE TABLE IF NOT EXISTS agent_card_settlements (
    id               TEXT PRIMARY KEY,                                 -- the Stripe event (evt_…): one per event
    kind             TEXT NOT NULL CHECK (kind IN ('capture', 'release', 'refund')),
    authorization_id TEXT NOT NULL DEFAULT '',                         -- iauth_…; '' for a refund Stripe did not link
    transaction_id   TEXT NOT NULL DEFAULT '',                         -- ipi_…, for a capture or a refund
    card_id          TEXT NOT NULL,
    workspace_id     TEXT NOT NULL,
    agent_id         TEXT NOT NULL,
    status           TEXT NOT NULL DEFAULT '',                         -- the authorisation's status, for a release
    amount_minor     BIGINT NOT NULL,                                  -- what Stripe says moved (or still holds), card currency
    currency         TEXT NOT NULL,
    hold_minor       BIGINT NOT NULL DEFAULT 0 CHECK (hold_minor >= 0),-- how much of the hold this settles
    hold_ulxc        BIGINT NOT NULL DEFAULT 0 CHECK (hold_ulxc >= 0),
    amount_ulxc      BIGINT NOT NULL,                                  -- what moved: + credited the agent, − debited it
    rate_date        DATE,                                             -- the rate converted at; NULL for USD
    ecb_usd_per_eur  NUMERIC,
    ecb_currency_per_eur NUMERIC,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_agent_card_settlements_authorization ON agent_card_settlements (authorization_id);
CREATE INDEX IF NOT EXISTS idx_agent_card_settlements_agent ON agent_card_settlements (agent_id, created_at DESC);

CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON agent_card_settlements
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON agent_card_settlements
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();
