# Step 2b — the allowance on the serving path: measured, and NOT built

**Status: a MEASUREMENT and a BRIEF. I started building this and stopped, and the reason is the
useful part.** The commit that adds this file changes no behaviour; it adds
`internal/proxy/allowance_reach_test.go`.

## 1. The allowance ledger has no reader

`billing.Service.Consume` has **zero production callers**. `billing.Service.CurrentAllowance` has
**none at all**. Step 2 shipped a table, a hard `CHECK`-constrained cap, a row lock and six
concurrency controls, and nothing on the serving path asks it anything — so a subscription creates a
row and changes nothing about what is served or what is charged, at any flag setting.

Step 2's own note discloses this ("It does NOT gate serving") and names this wiring as the natural
next merge. The disclosure is honest; what had not been written down is the shape of the wiring.

## 2. The two halves cannot ship separately

The obvious change is two lines in two places:

- **admission** — `lxcGateBlocks` currently returns `balance < estLXC`. A subscriber with a full
  allowance and no prepaid credit is therefore **refused 402**: a paid subscription buying nothing
  at the gate. It wants `balance + remainingAllowance < estLXC`.
- **debit** — the post-serve charge currently takes the whole cost from prepaid. It wants to spend
  allowance first and charge prepaid only the remainder.

⚠ **Either half alone is wrong, and wrong in a way that moves money.** Admit on allowance but debit
only prepaid, and every subscriber request drives the prepaid balance below zero by exactly the
covered amount. Charge allowance but admit only on prepaid, and a subscriber who has already paid is
refused. They are one merge.

## 3. ⚠ The trap — and it is why this is a brief and not a merge

The obvious place to spend the allowance is `shadowSpendLXC`. It is the function whose entire job is
"post-serve LXC debit", and the LXC gate's own header names it as the path that books the real cost.

**Both of its call sites sit in the `else` of `if p.reservationActive()`, and reservations are on by
default:**

```go
if p.reservationActive() { settleReservation(...) } else { shadowSpendLXC(...) }
```

`reservationActive()` is `LXCReservationEnabled && agentSpender != nil && LXCAgentAllocationEnabled`,
and `config.Load` sets **both flags true**. So an allowance wired only into `shadowSpendLXC` would be
**inert in the default configuration** — a money mechanism that looks wired, passes its own unit
tests, and never runs. That is the same defect class the allowance ledger is already in, one layer
down.

`TestMeasured_TheDefaultDebitPathIsTheReservationSettleNotTheShadowDebit` pins this.

## 4. What a builder has to answer first

The live path is `settleReservationBasis` / `settleReservation` — a **hold → settle/release**
reservation with clamping and refunds. Putting an allowance into it raises questions that have real
answers, none of which are in the code today:

- Does the **hold** reserve allowance, or only the settle spend it? A hold that does not reserve can
  be double-spent by concurrent requests; a hold that does reserve must **refund** allowance on
  release, and `Consume` has no inverse (deliberately — the cap is a `CHECK` constraint).
- The crash sweeper **releases** stranded reservations. What releases stranded allowance?
- `settleReservation` **clamps the settle to the hold**. If allowance covered part of the hold, what
  is clamped — the total, or the prepaid remainder?

Those are design decisions on a refundable money path. Guessing them is how a serving regression
ships, which is what step 2's own note warns about.

## 5. What I did build, and reverted

A complete two-half implementation: an `allowanceLedger` interface in `internal/proxy`, a
`RemainingAllowanceULXC` method on `billing.Service`, the gate term, the debit split, config-gated
wiring, and ten passing tests including "byte-identical when unwired" and "byte-identical when no
allowance row exists". **It was reverted on measuring §3** — every one of those ten tests passed
against a debit path that a default deployment does not take.

The tests were not wrong. They were pointed at the wrong branch.

## 6. Suggested order for whoever takes it

1. Measure the reservation path — hold, settle, release, the sweeper, the clamp.
2. Decide the three questions in §4 (they may need Nicolai; the refund one certainly touches money).
3. Build **both** halves against the path that actually runs, plus the shadow branch for the
   non-default configuration.
4. Keep the two inertness assertions: unwired, and no-allowance-row.
   `LENS_SUBSCRIPTION_ALLOWANCE_ULXC` defaults to 0, so the default deployment must be
   byte-identical.

## Controls

`w461-allowancereach-controls-k7v3.py` — **3/3 CAUGHT**. Both assertions here are **censuses that
assert an absence**, which is the shape most likely to be reporting its own blindness: "no caller
found" and "no caller exists" look identical from outside. Each mutation therefore *introduces* the
thing the census says is missing.

⚠ **Control G1 found a hole in the census that would have mattered.** The first version required the
calling file to import `internal/billing` — which excludes exactly the shape a real wiring takes,
because a serving-path reader calls the ledger through a **local interface** so that
`internal/proxy` need not import `internal/billing` for one struct. That file imports nothing from
billing, so the census reported "no production reader" with the reader in front of it. **My own
reverted implementation would have gone undetected by it.** The filter is now "mentions an
allowance", which still excludes `internal/attestation`'s unrelated `Consume`.
