# Tare phase 1d — the metering record: premises verified, and one of them is false

W6.1.4's own first instruction is a verification, so that is what this merge does. **No metering
record is built**, because the premise it rests on does not hold.

## ✗ Premise 1 — FALSE. The work-item id is not gateway-signed

> ⚠ THE WORK-ITEM ID COMES FROM THE GATEWAY-SIGNED HEADER a client cannot forge (the T7 attribution
> path Track already uses). ⚠ VERIFY THAT PATH EXISTS BEFORE DEPENDING ON IT.

**It does not exist.** The work-item id arrives as the `X-Talyvor-Feature` request header. Measured
across the whole tree: **five files read it with a plain `Header.Get`/`Header.Set`, and nothing
verifies it.** It is not signed, and a client can set it to anything.

**This repo already knows that, and already refuses — twice — to let it reach anything that scores
or mints:**

- `internal/mining/pattern_mining.go`: *"it is the one caller-controlled field (the
  `X-Talyvor-Feature` header), so keying rarity on it would let a workspace manufacture uniqueness
  by varying the header. feature_category is still PERSISTED on the row (analytics) — **it just
  cannot move the score**."*
- `internal/cohort/cohort.go`: *"feature_category is DECLARED at the boundary … **This package
  reaches no ledger/mint path**."*

### What *is* signed is something else

talyvor-track's `internal/lensintegration/webhook.go` HMACs the **webhook payload**, and its own
comment says the issue is matched by the `X-Talyvor-Feature` value *"SCOPED to the workspace the
signed payload names."* **The signature authenticates the workspace. The work-item id inside it is
still the caller's string.**

### Why that blocks the item as written

The record W6.1.4 specifies carries `delta_cost_usd` against `work_item_id`, and
`reports/tare-design-v1.html` says that figure is **avoided-COGS feeding the same economy** that
pays cache and distill royalties — justified precisely because the signal is *"gateway-observed, not
participant-asserted"*.

**The saving is gateway-observed. The attribution is participant-asserted.** Attributing a money
figure to a caller-declared string is exactly what the two files above already refuse to do. Doing
it here would contradict two deliberate, documented refusals in the same repository.

## ✓ Premise 2 — TRUE. `savings_pct` is still writerless

> ⚠ DO NOT USE token_events.savings_pct … "WRITERLESS — always 0, no writer has ever existed" …
> A guard pins it.

Verified: migration 0114 still carries the `WRITERLESS` comment, there is still no `INSERT`/`UPDATE`
writing it, and `internal/catalog/writerless_column_guard_test.go` still pins it. The instruction to
write a **new** column and ship the writer and reader together stands.

## The decision this needs

**Not "is the header forgeable" — that is measured. The decision is what a savings figure may be
attributed to.**

1. **Attribute to the declared field anyway, and never let it reach money.** Persist
   `feature_category` on the metering row for analytics — the posture `cohort.go` already takes —
   and make the economy-facing figure workspace-scoped only. Cheapest, contradicts nothing, and the
   suite cannot then say *"Tare saved $X on TALYVOR-123"* with a number anyone should trust.
2. **Build a real signed attribution.** A gateway-minted, workspace-scoped work-item token the
   client cannot forge. This is what the design assumes already exists. It is a new credential
   surface and is squarely a product decision.
3. **Drop per-work-item attribution from phase 1.** Ship the gateway-observed half — `tokens_in`,
   `tokens_out`, `delta_cost_usd`, `reduction_kind`, `holdout_arm`, all unforgeable — and treat
   per-work-item savings as a phase-2 feature gated on (2).

⚠ **Option 1 is the one that looks free and is not**, because the design sells exactly the sentence
it cannot support: *"Tare saved $X on TALYVOR-123"* is the moat paragraph.

## What is still buildable today, and the trap in it

Everything except `work_item_id` is gateway-observed and unforgeable. But note before building:

⚠ **`holdout_arm` must be "recorded ON THE BILLING WRITE", and the billing write has two paths.**
`docs/model2-step2b-brief.md` measures it: `shadowSpendLXC` looks like the post-serve debit and both
its call sites sit in the `else` of `if p.reservationActive()`, with **both governing flags default
true**. A holdout arm recorded only there would be **absent in the default configuration** — a
metering column that is always null, which is how the last one rotted.

## What is merged here

The verification, executable, so it is re-checked rather than re-argued:

- `TestPremise_TheWorkItemHeaderIsNeitherSignedNorVerified` — **two-sided**: it also requires the
  header to actually *be read*, so "nothing verifies it" can never be a statement about the search.
  **Expected to go red the day a real signature check appears** — which is the day premise 1 becomes
  true and this brief's blocking half can be deleted.
- `TestPremise_TheRepoAlreadyRefusesToLetTheDeclaredFieldScoreOrMint` — reads the actual text of
  both refusals, independently, so one going stale cannot hide the other.
- `TestPremise_SavingsPctIsStillWriterless`.

Controls: `w614-premise-controls-k7v3.py`, **4/4 CAUGHT**.

⚠ **K2 mutates the test, not the product, and that is correct for it.** It proves the anti-blindness
floor, and the only way to reach that state is an empty population. Its first version removed one of
the five readers, left four, never tripped the floor, and reported MISSED while the floor was
working fine — a control too small to reach the thing it was checking.
