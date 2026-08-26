# `/v1/marketplace/listings/{id}/buy` takes no payment — re-measured, and made executable

W6.3.2, re-measured at current main. **Nothing is fixed.** Taking payment is a product design — what
does a buyer pay *with*? — and this is a live money path.

## What happens on a buy

`ExecuteTrade` performs **three ledger credits and no debit**:

| party | movement |
|---|---|
| buyer | `CreditTx(buyerID, netToBuyer, "marketplace_buy")` |
| Talyvor | `CreditTx(TalyvorWorkspace, fee, "marketplace_fee")` |
| seller | `CreditTx(sellerID, **unsold**, "marketplace_unsold_refund")` |

The seller was debited the **full** listing amount at `CreateListing` (an escrow). On a trade they
get back only the **unsold remainder** — there is no credit anywhere for the portion that sold. The
buyer receives LENS and **pays nothing**.

`amount_usd` comes from the request body, and `tradeSplit` turns it into
`lensAmount = amount_usd / price_usd`, capped at the listing size. **A caller names how much of the
seller's escrowed LENS they receive.**

## ⚠ The clearest single symptom

`cmd/lens/main.go` maps `economy.ErrInsufficientBalance` → **402 Payment Required** on the buy route.

**`ExecuteTrade` cannot return it.** The only producer is `CreateListing`, for the *seller's* escrow
debit. It is a status code for a payment that is never taken — the handler's author expected a
balance check that does not exist.

## Why no existing guard catches it

Conservation **holds**: `netToBuyer + fee + unsold == listingAmount`, and `marketplace_buy` is
correctly outside `CountedSupplyTypes()`. **Nothing is minted.** The defect is missing
**consideration**, and no conservation invariant can see that — *a free transfer conserves
perfectly*. That is why this needed measuring rather than a supply check.

## What is NOT wrong, so nobody re-worries it

`buyer_workspace` is also in the request body, and this route has no `{wsID}` path segment so
`workspaceIsolationMiddleware` cannot apply — but `effectiveWorkspaceID` **is** called and forces the
buyer to the caller's own workspace for non-admins ("the buyer is the CALLER for non-admins; admin
honors the body", closed by #146). **A caller cannot credit someone else's workspace.**

## Exposure

- **Routes are registered by default** — `econ.post(...)` with `econReg{on: cfg.EconomyEnabled}` and
  `c.EconomyEnabled = true`.
- **No UI in any repo**: `talyvor-suite`, `talyvor-docs`, `talyvor-track`, `talyvor-code` reference
  `marketplace` in **0 files each**. Reachable only by a direct Lens API call, and only if listings
  exist.

⚠ This is the **same exposure shape as W6.3.3's staking routes**: on by default, no client, live money
path. **Whether this route family should be registered at all is one question, not two.**

## The decision

Taking payment means choosing what a buyer pays with — prepaid LXC, a Stripe charge, or an existing
LENS balance — and each has a different failure mode on a partial fill. Not a session's call.

Until it is decided, the cheapest safe option is **not registering the route family**, which also
answers W6.3.3. That is a config change, and it is Nicolai's.

## What is merged

The finding, executable, so it cannot persist silently:

- `TestMeasured_ExecuteTradeCreditsThreePartiesAndDebitsNobody` — **two-sided**: `.Debit(` must still
  appear elsewhere in the file, or "zero debits" would be a statement about the search string.
- `TestMeasured_TheSellerIsNeverPaidForTheSoldPortion`
- `TestMeasured_TradeSplitHasNoBuyerCostOutput` — the finding stated structurally: **the function
  that computes the trade returns four LENS quantities and no consideration.**
- `TestMeasured_TheBuyHandlerMapsA402ThatExecuteTradeCannotReturn`

**All four are expected to go red when payment is taken** — that is the fix landing, and each failure
message says so.

⚠ **A source census, not a `pgxmock` test, and deliberately.** The neighbouring tests drive a mock
pool, which means declaring the SQL you expect — and "assert the statements I think it runs" is a
fixture asserting itself when the claim under test is that a statement is **absent**.

Controls: `w632-consideration-controls-k7v3.py`, **4/4 CAUGHT**, sha256-verified restores after every
mutation (this file is a money path). Each mutation **simulates the fix** — a buyer debit, a seller
proceeds credit, a `buyerPays` return value, a reachable `ErrInsufficientBalance` — and requires the
census to notice.
