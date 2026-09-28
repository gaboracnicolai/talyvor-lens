-- B19.15 — an agent pays another company's agent through the marketplace.
--
-- The payment is one billed market_uses row with no listing: its buyer is the paying company, its agent the
-- paying agent, its seller the company whose agent is paid (payee_agent_id). It goes onto the paying
-- company's monthly marketplace bill like any use, within the paying agent's rules; when that bill is paid
-- the payee's company earns it, payable after the 14-day holdback and paid out through B20.5. The
-- single-party detector refuses a payment between two companies that share a card or an owner.
ALTER TABLE market_uses
    ADD COLUMN IF NOT EXISTS payee_agent_id TEXT NOT NULL DEFAULT '',  -- '' for a use of a listing
    ADD COLUMN IF NOT EXISTS memo           TEXT NOT NULL DEFAULT '';
