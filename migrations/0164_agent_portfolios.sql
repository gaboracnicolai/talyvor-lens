-- B22.8 — investing and trading, simulated until a broker partner exists.
--
-- An agent holds simulated portfolios and places market and limit orders on real instruments at real prices,
-- executed by Talyvor's simulator — never sent to any market. The instruments are the currencies the European
-- Central Bank publishes euro foreign exchange reference rates for (ecb_reference_rates, B19.12), priced in US
-- dollars from the latest published day: USD per unit = (USD per EUR) ÷ (unit per EUR). The ECB allows its
-- statistics to be reused, commercially too, when it is cited as the source.
--
-- A portfolio's cash is simulated US dollars, set when it is opened — never credits, never money. Its cash and
-- positions are derived from its filled orders, never stored. A market order fills at once at the quote; a
-- limit order fills when the quote crosses its limit (a buy at or under it, a sale at or over it), at the
-- quote, on the tick after a new day's rates. Live trading is class RED (invest_and_trade): only ever through
-- an authorised broker partner, never by Talyvor itself.
CREATE TABLE IF NOT EXISTS agent_portfolios (
    id                 TEXT PRIMARY KEY,                                  -- pf_<uuid>
    workspace_id       TEXT NOT NULL,
    agent_id           TEXT NOT NULL,
    name               TEXT NOT NULL,
    starting_cash_uusd BIGINT NOT NULL CHECK (starting_cash_uusd > 0),    -- simulated µUSD
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (agent_id, name)
);
CREATE INDEX IF NOT EXISTS idx_agent_portfolios_agent ON agent_portfolios (workspace_id, agent_id);

CREATE TABLE IF NOT EXISTS agent_sim_orders (
    id              TEXT PRIMARY KEY,                                     -- ord_<uuid>
    portfolio_id    TEXT NOT NULL REFERENCES agent_portfolios (id) ON DELETE CASCADE,
    instrument      TEXT NOT NULL,                                        -- ISO 4217 code, e.g. EUR
    side            TEXT NOT NULL CHECK (side IN ('buy', 'sell')),
    type            TEXT NOT NULL CHECK (type IN ('market', 'limit')),
    quantity_micros BIGINT NOT NULL CHECK (quantity_micros > 0),          -- units of the instrument × 10⁶
    limit_price     NUMERIC CHECK (limit_price > 0),                      -- USD per unit, for a limit order
    status          TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'filled', 'cancelled', 'rejected')),
    fill_price      NUMERIC,                                              -- USD per unit it filled at
    fill_rate_date  DATE,                                                 -- the ECB day that price is from
    cash_uusd       BIGINT NOT NULL DEFAULT 0,                            -- what it moved in cash: − bought, + sold
    reason          TEXT NOT NULL DEFAULT '',                             -- why it was rejected
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at      TIMESTAMPTZ,
    CHECK ((type = 'limit') = (limit_price IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS idx_agent_sim_orders_portfolio ON agent_sim_orders (portfolio_id, created_at);
CREATE INDEX IF NOT EXISTS idx_agent_sim_orders_open ON agent_sim_orders (created_at) WHERE status = 'open';
