# `lifetime_earned` is lifetime CREDITED (W4.6.1 step 7)

Measured 2026-08-26 (tab-h3n8) at main `cbf2dbf`, on a from-zero `pgvector/pgvector:pg16`, through
the production ledger. **Not fixed** — the last section says why, and what the decision is.

## Why this was looked at

W4.6.1 step 7 asks for the surface behind *"your plan is $20, your answers earned $6 of it back."*
`lens_token_balances.lifetime_earned` is the field anyone building that reaches for. It is named
earned; it is already served on `GET /v1/workspaces/{wsID}/tokens/balance` as
`lifetime_earned_ulens`; and `cmd/node/earnings.go` and `cmd/cachenode/main.go` already read it.

## The finding

`LedgerStore.applyTx` does, for every credit:

```go
if add {
    delta = amount
    earned += amount        // ← no filter on txType
}
```

There is no filter on the ledger type. LENS a workspace was **given**, **bought**, or simply got
**back** all raise `lifetime_earned`.

`Credit`'s own doc comment states the assumption that would make this safe —

> A credit is LENS entering circulation (mining rewards). Transfers move between workspaces via
> Transfer (not Credit), so this doesn't double-count.

— and `internal/economy/marketplace.go` breaks it **five times**, calling `CreditTx` directly for
`unstake`, `marketplace_buy`, `marketplace_fee`, `marketplace_unsold_refund` and
`marketplace_refund`. `apply` only wraps `applyTx` in a Begin/Commit, so both doors reach the same
line, and the measurement drives each type through the door production uses for it.

### Measured, not read

One workspace, five credits through the real production path:

| ledger type | µLENS | earned? |
|---|---:|---|
| `cache_mine` | 1,000,000 | yes |
| `transfer_in` | 5,000,000 | no — somebody sent it |
| `marketplace_buy` | 7,000,000 | no — it *bought* the LENS |
| `marketplace_unsold_refund` | 3,000,000 | no — its own escrow returned |
| `unstake` | 11,000,000 | no — its own principal returned |

```
MEASURED: lifetime_earned=27000000 µLENS; genuinely earned=1000000 µLENS;
          overstatement=26000000 µLENS (27.0×)
```

### And it is unbounded, with no counterparty

Staking your own LENS and unstaking it returns you to exactly the balance you started with. Five
value-neutral cycles:

```
MEASURED: 5 value-neutral cycles raised lifetime_earned 4000000 → 24000000 µLENS
          while the balance stayed at 4000000. The field is unbounded in the number of round trips.
```

`stake` debits (`spent += amount`), `unstake` credits (`earned += amount`). Nothing was earned,
nothing was created, and the field grows by the principal every cycle. There is no cap, because a
cap on a value-neutral loop is not something anyone thought to write.

## What is NOT wrong, so nobody re-worries it

- **Total supply is fine.** `GetTotalSupply` uses an explicit allow-list (`countedSupplyTypeList`)
  and `WHERE amount > 0`, so `unstake` / `marketplace_*` / `transfer_in` are excluded there. The
  defect is confined to the per-workspace `lifetime_earned` column.
- **`metrics.MintedTokens` is not affected by the marketplace path.** `Credit` emits it; `CreditTx`
  — which is what `internal/economy` calls — does not. Checked before it was written down.
- **`held_balance` is correct.** `heldInner` decrements it on both finalize and revoke, so it is a
  current balance rather than an all-time total. ⚠ Summing `*_held` LEDGER ROWS would *not* be:
  finalize writes a positive `pool_royalty` row and does **not** write a negative `pool_royalty_held`
  row, so `SUM(amount) WHERE type='pool_royalty_held'` is everything ever held, including everything
  already paid out. Any earnings surface must read the **column**, not the rows.
- **`povi_stake_lock` / `povi_stake_release` do not inflate it.** `stake_ledger.go` has its own
  balance update that never touches `lifetime_earned`. Only `internal/economy`'s `stake`/`unstake`
  pair does.

## Why it is not fixed here

`lifetime_earned` is **already served** and **already read by two node binaries**. Narrowing it
changes a number those binaries report and a field an API client may already be charting. And
*which* types count as earned is not an engineering detail — it is the same product question step 7
is asking:

- Is `pool_royalty` alone "your answers earned"? Or every counted-supply mint?
- Does `stake_yield` count? It is settled, counted supply, and credited to the workspace — but
  nobody wrote an answer for it, and W6.3.3 has it under an open economic and legal question.
- Is the fix to narrow `lifetime_earned`, or to leave it (renamed, honestly, as *credited*) and add
  a separate earnings figure beside it?

`internal/earnings` in this PR is the instrument for whichever answer is chosen: it classifies all
43 live ledger types into settled / held / revoked / not-earnings, each with its reason, and splits
settled income into **contribution** and **capital** so that "your answers earned this" can be true
of a subtotal rather than approximately true of a total.

## The census boundary, stated

The ledger's type vocabulary spans **three packages in two representations**: 35 exported `Type*`
constants in `internal/mining`, one more in `internal/povi` (which `internal/mining/mint_gate.go`
already duplicates as a cycle-free literal, saying so), and **seven bare string literals** in
`internal/economy` with no constant at all. `TestE1` derives its population from the two packages
that use constants; `TestE2` walks `internal/economy`'s ledger call sites for the literals; the
`Unclassified` bucket is what catches anything both miss.

⚠ **The literal count was five until the AST walk corrected it, and that is the argument of this
file made at my own expense.** The first version of the map was built from a grep for the type
argument on the same line as the ledger call. `marketplace_listing` and `marketplace_refund` pass
theirs on the *following* line, so the grep could not see them — a 29% undercount, in a census
written by someone who had that same morning reported a hand-maintained guard for being blind to
new members. A hand-built population is wrong in the direction nobody checks.
