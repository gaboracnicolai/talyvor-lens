# Investing and trading — simulated

B22.8. An agent can hold portfolios and place market and limit orders on real instruments at real prices.
**Everything here is simulated.** Orders are executed by Talyvor's simulator and never sent to any market.
A portfolio's cash is simulated US dollars fixed when it opens: no credits and no money ever move. Live trading
is class RED (`invest_and_trade`, see [wallet capabilities](wallet-capabilities.md)). It happens only through
an authorised broker partner, never through Talyvor itself, so an order asking for any mode but `simulated` is
refused naming the class.

## Market data

The instruments are the currencies the **European Central Bank** publishes its euro foreign exchange
reference rates for (about thirty, and the euro itself). Each is priced in US dollars from the latest
published day: USD per unit = (USD per EUR) ÷ (unit per EUR). Lens already fetches the ECB's daily file for
card pricing (B19.12); it now does so in every deployment, at start and every three hours.

Why this source: the ECB allows its statistics to be reused, commercially as well, provided the ECB is cited
as the source (and, where the information is sold, buyers are told it is free at www.ecb.europa.eu). Every
quote and portfolio carries `market_data` naming it. Exchange and broker feeds such as Coinbase's public API
forbid showing their data to anyone outside the operator's own organisation, so they could not be used here.

The reference rates are published once a working day, around 16:00 CET. The simulator's prices therefore move
once a day, and an open limit order fills on the first tick after a day's rate crosses its limit.

## Orders

- **market:** fills at once at the quote.
- **limit:** a buy fills when the quote is at or under `limit_price_usd`, a sale when it is at or over it,
  always at the quote. If the quote already crosses when it is placed, it fills at once; otherwise it stays
  open until a later day's rate crosses (on the agent schedules' tick), or until it is cancelled.

A purchase needs the simulated cash — at the limit, for an open limit order — and a sale needs the units. A
limit order the portfolio can no longer cover when it crosses is `rejected`, saying why. Quantities are in
millionths of a unit (`quantity_micros`) and cash in µUSD. A purchase's cost is rounded up and a sale's
proceeds down.

## API

```
GET  /v1/markets/simulated/quotes
GET  /v1/workspaces/{ws}/agents/{agent}/portfolios
POST /v1/workspaces/{ws}/agents/{agent}/portfolios                     {"name": "fx", "cash_uusd": 10000000000}
GET  /v1/workspaces/{ws}/agents/{agent}/portfolios/{pf}                cash, positions and value at the quotes
POST /v1/workspaces/{ws}/agents/{agent}/portfolios/{pf}/orders         {"instrument": "EUR", "side": "buy",
                                                                        "type": "limit", "quantity_micros": 1000000000,
                                                                        "limit_price_usd": "1.15"}
POST /v1/workspaces/{ws}/agents/{agent}/portfolios/{pf}/orders/{order}/cancel
```

Each takes the agent's own key, the workspace's owner or an admin. A portfolio's `value_uusd` is its cash
plus every position at its quote. Its `orders` are every order, open and decided, with the price and ECB day
each filled at.
