# Talyvor's share is not credited to a house account, and should not be

**Decision:** do not create a treasury / house / platform balance. Answer "what did pooling earn
this month" with a **derived view over existing rows**.

**Status:** decided. Nothing to build in the ledger; the query below is the deliverable.

---

## The observation that prompted it

`internal/poolroyalty/minter.go` says Talyvor **nets** (1−s) × avoided_COGS. *Nets*, not *credits*.
Searching the repository for a treasury, house or platform account returns nothing — the single
match is a comment in `migrations/0086_provenance_bonds.sql` stating that a slashed bond is
*"paid to NOBODY — no counterparty, no treasury"*, which is a deliberate position, not an oversight.

So the margin on a pooled hit exists as LXC charged to the consumer and never paid out. It is real
in Stripe and in no ledger of ours.

## Why a house account would be wrong

**1. It would credit ourselves a liability, not an asset.** LXC is prepaid credit we issue: every
µLXC outstanding is a promise to serve inference. When a consumer spends LXC, that liability is
*discharged* — which is precisely the moment the revenue is earned. Crediting a house LXC balance
would record us as *holding* the thing we just stopped owing. A balance denominated in our own
credit is not income; it is a claim against ourselves.

**2. It would double-count the cash.** The money arrived once, at Stripe, and is already
represented: cash in (asset) against LXC issued (liability). Recognising revenue as the liability
is consumed completes that entry. A third row crediting a house account records the same economic
event a second time, in a unit that cannot be reconciled to the first.

**3. Every reader would then owe an exclusion.** Supply figures, balance sums and the LENS/LXC
totals all query the ledgers. A house row inside them silently inflates each one, so every existing
query would acquire an obligation to exclude it — an N-reader obligation, which is exactly the
shape `internal/mining/shadow.go` rejected when it put shadow mints in a *different table* to
convert the same problem into a one-writer obligation. Adding a house balance walks that back.

**4. Not all charged LXC is revenue, and a balance cannot tell the difference.** A pooled hit paid
for with **comped** LXC generates no cash. The margin there is notional: we discounted a grant we
issued. A single house balance conflates granted and purchased spend into one number that reads as
earnings; a derived view can separate them by funding source, because the source is still on the
rows.

## What to do instead

Everything needed is already recorded. The suite's own reporting can compute it:

| fact | where it lives |
|---|---|
| what the consumer paid for a pooled hit | `lxc_ledger` spend row: `amount`, plus `pool_list_ulxc`, `pool_saved_ulxc`, `pool_discount_rate` in `metadata` |
| what the contributor was paid | `pool_royalty_mints.minted_amount`, and the matching `lens_token_ledger` credit |
| who the two parties were | `pool_royalty_mints.requester_workspace_id` / `contributor_workspace_id` |
| the basis the royalty was funded from | `pool_royalty_mints.avoided_cogs_usd` |

```sql
-- What pooling earned in a window, from rows that already exist.
--
-- Talyvor's take is the consumer's charge minus the contributor's royalty. Both sides are
-- recorded per request, so this needs no new balance and cannot drift from the ledgers — it IS
-- the ledgers.
--
-- ⚠ The two sides are in different units: the charge is µLXC, the royalty is µLENS. They are
-- reconciled through avoided_cogs_usd, the funding basis the mint was computed from, which is why
-- that column is on the claim row. Do not subtract µLENS from µLXC.
SELECT
    date_trunc('month', l.created_at)                      AS month,
    count(*)                                               AS pooled_hits,
    sum(-l.amount)                                         AS charged_ulxc,
    sum((l.metadata->>'pool_saved_ulxc')::bigint)          AS discounted_to_consumers_ulxc,
    sum(coalesce(m.minted_amount, 0))                      AS paid_to_contributors_ulens,
    sum(coalesce(m.avoided_cogs_usd, 0))                   AS royalty_basis_usd
FROM lxc_ledger l
LEFT JOIN pool_royalty_mints m
       ON m.request_id = l.metadata->>'request_id'
WHERE l.type = 'spend'
  AND l.metadata ? 'pool_list_ulxc'          -- the marker that this charge was a POOLED hit
GROUP BY 1
ORDER BY 1 DESC;
```

**The caveat that keeps this honest:** `charged_ulxc` is spend, not cash. To answer "what did
pooling earn in *money*", the granted portion must be excluded — join the consuming workspace to
`lxc_purchases` and treat spend against comped credit separately. A house balance would have hidden
that distinction behind a single number; the query is forced to confront it, which is the better
failure mode.

## What would change this decision

If Talyvor ever needs to **hold and spend** its own LXC — funding grants from margin, or paying a
contributor out of the house rather than by minting — then a house account stops being a
double-count and becomes a real position with real movements. That is a different system from the
one described here, and the reopening condition for this decision.
