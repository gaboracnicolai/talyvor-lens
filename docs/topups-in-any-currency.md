# Top-ups in any currency (B22.2)

Credits are always bought in US dollars. The top-up Checkout Session enables Stripe's
**Adaptive Pricing**, so a customer in any of Stripe's supported countries sees and pays the price in
their own currency. Stripe converts it at its own rate, and the customer pays Stripe's conversion fee
(2–4%). Wallets hold USD only, and Lens keeps no exchange-rate logic for top-ups.

**Requirement.** Adaptive Pricing needs the prices' currency (USD) to be the Stripe account's default
settlement currency. This was checked on 29 Sep 2026: Talyvor's account is GB and its
`default_currency` is `usd`. If that changes, check the top-up checkout still opens before relying on it.

**What Lens records.** When Stripe localises a session, `checkout.session.completed` carries:
- the customer's currency and amount, as `currency` and `amount_total`;
- the USD it was converted from, in `currency_conversion`.

Lens credits the USD figure from `currency_conversion`, at the peg (1 LXC = $0.10). The top-up's
`lxc_ledger` row keeps what the customer actually paid, as `paid_currency`, `paid_amount` and
`stripe_fx_rate`, beside `usd_cents`.

A session created in a currency other than USD with no conversion is still refused as anomalous, and
is never credited.
