# `stake_positions` — what it actually mints, measured

W6.3.3. **Nothing is changed and nothing is tuned.** The item is explicit: *"MEASURE FIRST AND
REPORT … That measurement is the item; do not tune the APY"* and *"DO NOT CHANGE THE APY, THE LOCK
TIERS OR ANY THRESHOLD"*. The APY constants, the lock tiers and the yield formula are read here,
never written.

## The three numbers the item asked for

### 1. Implied mint at the current APYs — computable exactly

| lock | advertised APY | mint per lock term |
|-----:|---------------:|-------------------:|
| 30d | 5% | **0.4109%** of principal |
| 90d | 12% | **2.9589%** of principal |
| 180d | 20% | **9.8630%** of principal |

### 2. ⚠ But the accrual is **not capped at the lock term**, so that table is a floor

The unstake path computes:

```go
yield := computeYield(pos.Amount, pos.APY, time.Since(pos.StartedAt))
```

`time.Since(pos.StartedAt)` — elapsed since the position **started**, with **no ceiling at
`unlocks_at`**. The lock is a **minimum hold, not a maximum accrual window**.

**A 180-day position at 20% mints 9.86% of principal if unstaked the day it unlocks — and 100% of
principal if it is left for five years.** The implied mint per position is unbounded in time and
grows at the full advertised rate indefinitely.

Whether that is intended is a product question: an APY *is* an annual rate, so accruing at 20%/yr
forever is one reading — but `lock_days IN (30, 90, 180)` plus `unlocks_at` frames this as a
**fixed-term** instrument, and a fixed-term instrument that keeps paying its headline rate after
maturity is unusual. **Not changed here.**

### 3. Total staked, and share of supply — ⚠ NOT MEASURABLE ON THIS BOX, and that is the honest answer

There is **no production database reachable from this machine**: the compose stack
(`talyvor-lens-postgres-1`) is **not running**, and there is no `LENS_DATABASE_URL` in `.env`. So
`SELECT sum(amount) FROM stake_positions` cannot be run, and any "total staked" or "share of supply"
figure would be invented.

What *can* be given is the formula the decision rests on, because the structural half is measured:

> `stake_yield` **is** in `CountedSupplyTypes()`, so every µLENS of yield adds directly to
> `GetTotalSupply`. If a fraction **s** of counted supply is staked at APY **a**, annual dilution of
> every other holder is approximately **s × a**.

At the 180-day tier that is **0.20 × s per year**. Someone with database access can finish this
report by running one query.

## Exposure — measured

- **The routes are registered by default.** `econReg{on: cfg.EconomyEnabled}` and
  `c.EconomyEnabled = true` is the default, so `POST /v1/workspaces/{wsID}/tokens/stake`,
  `…/stake/{positionID}/unstake` and `GET …/tokens/stakes` are live in a default deployment.
- **There is no UI, in any repo.** `talyvor-suite`, `talyvor-docs`, `talyvor-track` and
  `talyvor-code` reference a stake path in **0 files each** (positive-controlled by the same search
  finding the Lens routes). Reachable only by a direct Lens API call — the same shape as W6.3.2's
  marketplace finding.

## The good news, so the report is not one-sided

**The mint is counted.** `TypeStakeYield` is in `countedSupplyTypeList`, so the dilution is visible
in `GetTotalSupply` rather than hidden. The unstake path's own comment records that this was *not*
always true — it used to write principal+yield under an uncounted `unstake` label, so "the YIELD was
real LENS in a wallet that GetTotalSupply never saw", found by #400's sweep and since fixed. That
fix holds, and `TestMeasured_StakeYieldIsCountedInTotalSupply` now pins it.

## The two decisions, unchanged from the item

- **ECONOMIC** — the yield is a mint with no revenue behind it. The measurement above gives the rate;
  the absolute needs one SQL query on a real database.
- **LEGAL** — a locked deposit paying a fixed advertised return is the textbook shape of a security
  in several jurisdictions, independent of redeemability. ⚠ **This needs a lawyer, not an engineer**,
  and until one has looked, whether these routes should be registered at all is worth asking — they
  are on by default and nothing in the product uses them.

## What is merged

`internal/economy/stake_exposure_test.go` — the facts, executable, so they are re-measured each CI
run instead of resting on this document.

- `TestMeasured_StakeYieldIsNotCappedAtTheLockTerm` — **expected to go red when the window is
  capped**, which is the fix landing; its failure message says so.
- `TestMeasured_ImpliedMintPerLockTerm` — reads the **real** APY constants, so if any tier moves,
  every figure above is flagged stale rather than quietly restated.
- `TestMeasured_StakeYieldIsCountedInTotalSupply`.

Controls: `w633-stake-controls-k7v3.py`, **4/4 CAUGHT**, sha256-verified restores (no APY or lock
tier is left mutated — checked after every run).

⚠ **L1 found that the headline test could not see the thing it named.** Its first version exercised
`computeYield`'s arithmetic directly, so capping the *unstake path's* window — **which is the fix** —
left it green. The window is chosen at the call site, so the test now reads the call site.
