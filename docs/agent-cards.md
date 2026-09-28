# Agent cards (B19.12) — test mode only

Each agent can hold one virtual card, issued through Stripe Issuing. Lens approves or declines every
purchase made with it **in real time**, by the agent's spending rules (B19.2) and its balance. Every
purchase request, approved or declined, is written to the ledger.

Cards are **class RED** (B22): test money only until a licensed partner issues them. With a live Stripe
key Lens issues no card, and it declines any purchase made in live mode.

## Turning it on (Stripe test mode)

1. In the Stripe Dashboard, switch to **test mode** and open **Issuing**. Stripe offers local Issuing to
   UK platforms, and UK cards are issued in GBP.
2. `LENS_STRIPE_SECRET_KEY` in `/etc/talyvor/lens.env` must be the **test** key (`sk_test_…`).
3. Issuing → settings → **real-time authorisations**: set the endpoint to
   `https://<lens host>/v1/agent-cards/authorizations`, then copy its signing secret into
   `LENS_STRIPE_ISSUING_WEBHOOK_SECRET` in `/etc/talyvor/lens.env`. This endpoint is separate from the
   billing webhook and has its own secret.
4. `LENS_STRIPE_ISSUING_CURRENCY` defaults to `gbp`, which is right for a UK platform.
5. Restart Lens. It fetches the ECB reference rates at start and every three hours after that.

## Using it

The workspace's owner issues a card for an agent. The owner is the cardholder, because they answer for
the agent (B19.11):

```
POST /v1/workspaces/{ws}/agents/{agent}/card
{"first_name":"…","last_name":"…","email":"…","line1":"…","city":"London","postal_code":"…","country":"GB"}
```

To read the card and every authorisation on it, newest first:

```
GET /v1/workspaces/{ws}/agents/{agent}/card
```

To make a test purchase, use Dashboard → Issuing → the card → **Create test purchase**, or run
`stripe test_helpers issuing authorizations create --card ic_… --amount 2000`.

## What happens on each purchase

Stripe sends `issuing_authorization.request` and waits up to 2 seconds. Lens then does the following:

- **Pricing.** Lens converts the amount (in the card's currency) to USD at the day's ECB reference rate,
  meaning the latest rate published on or before the purchase date. The ECB publishes each currency per
  EUR, so the formula is USD = amount × (USD per EUR) ÷ (GBP per EUR), rounded up to the micro-dollar.
  The USD amount becomes LXC at the peg (1 LXC = $0.10).
- **Judging.** The agent's rules judge the purchase as a payment, under the agent's row lock: active
  hours, the limit per payment, the daily and monthly limits, pause, and the approval amount. A
  purchase above the approval amount is declined and files an approval. Once the owner approves it,
  the same purchase (same card, merchant and amount) goes through once. The agent's balance must also
  cover the purchase.
- **Recording.** Every request becomes one row of `agent_card_authorizations`, whether it was approved
  or declined, and even when the card is unknown to Lens. The row holds the reason and the ECB rate
  and date used. An approved purchase also writes, in the same transaction:
  - one `lxc_ledger` row of type `agent_card` off the workspace's balance;
  - one `agent_postings` entry of kind `card`.

  The agent's balance, the workspace's balance and the rules' day and month totals all count it.
- **Failures.** Anything that goes wrong after the signature is verified is answered as a decline,
  never an error. On an error, Stripe would fall back to its timeout setting, which may approve.

Not built yet: captures that differ from the authorised amount, and reversals or expiries that should
give an agent its money back. Stripe reports those as `issuing_transaction.*` and
`issuing_authorization.updated` events, which this endpoint only acknowledges.
