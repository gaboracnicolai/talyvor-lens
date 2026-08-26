# Tare phase 1d — the metering record: premises verified, and one of them is false

W6.1.4's own first instruction is a verification, so that is what this merge does. **No metering
record is built**, because the premise it rests on does not hold.

## ✗ Premise 1 — FALSE. The work-item id is not gateway-signed

> ⚠ THE WORK-ITEM ID COMES FROM THE GATEWAY-SIGNED HEADER a client cannot forge (the T7 attribution
> path Track already uses). ⚠ VERIFY THAT PATH EXISTS BEFORE DEPENDING ON IT.

**It does not exist.** That conclusion is unchanged. What was corrected afterwards is **which
header this brief was talking about**, and the correction matters more than it sounds.

### ⚠⚠ The work-item id is `X-Talyvor-Issue` (and `X-Talyvor-PR`). It is NOT `X-Talyvor-Feature`.

This brief's first version said the work-item id arrives as `X-Talyvor-Feature`. **It does not.**
`reports/tare-design-v1.html` books the saving to *"the exact issue / PR / spec it came from"*, and
on this repo's serve path those arrive as `X-Talyvor-Issue` and `X-Talyvor-PR`:
`attribution.ExtractFromRequest` maps them to `IssueID` / `Git.PRNumber`, and `recordSQL` writes
them to `request_attribution.issue_id` / `pr_number` — the row `internal/api`'s spend-by-request
endpoint LEFT JOINs to `token_events` for Track. `X-Talyvor-Feature` is the **declared category**:
`proxy.go` feeds it to alert rules and `token_events.feature`, and `ExtractFromRequest` even strips
a `"code-"` prefix from it *"to keep the dashboard chips readable"* — an IDE affordance, not an
identifier.

**⚠⚠⚠ This repository already shipped that exact confusion as a product defect and wrote the
post-mortem into `migrations/0116_request_attribution_request_id.sql`:**

> *"request_attribution already stores the issue the user was working on (issue_id, from the
> **X-Talyvor-Issue** header the Code extension sends) … Track credits an issue by matching the
> spend record's **FEATURE** against an issue identifier, and the extension sends the feature as an
> IDE affordance ("code-chat"), so **every request from the editor we ship attributed to nothing in
> the tracker we ship** — even though the issue was known and stored the whole time."*

### Why the correction was not cosmetic — it was measured

`internal/tare/attribution_premise_test.go` exists to go **red the day a real signature check
appears**, which is the event that lets the blocking half of this brief be deleted. Watching the
wrong header made it wrong in both directions, and both were measured with a positive control rather
than argued:

- **A gateway signature check added to `X-Talyvor-Issue` left the guard GREEN** (control X1b), and
  it logged *"the work-item id is caller-declared"* over a tree in which it no longer was. The
  instrument could not see the one event it exists to detect, so the block would never have lifted.
- A signature on `X-Talyvor-Feature` would have redded it with *"W6.1.4's premise has become true"* —
  **false**: signing a category authenticates no work item.

The guard now watches each header separately, with a **per-header** blindness floor rather than a
union — `X-Talyvor-Feature` has four reader files and `X-Talyvor-Issue` has one, so a union floor is
satisfied by Feature alone and would keep reporting a clean result after the work-item header
stopped being read at all (control X5 measured exactly that).

### The measurement, re-run against the right headers

| header | role | non-test files reading it | verified anywhere? |
| --- | --- | --- | --- |
| `X-Talyvor-Issue` | **work item** | 1 | **no** |
| `X-Talyvor-PR` | **work item** | 2 | **no** |
| `X-Talyvor-Feature` | declared category | 4 | **no** |

No `X-Talyvor-*` attribution header has an HMAC or signature check anywhere in this repository. So
W6.1.4's *"gateway-signed header a client cannot forge"* **still does not exist**, the premise is
still false, and everything below stands unchanged.

**This repo already knows that, and already refuses — twice — to let the declared category reach
anything that scores or mints:**

- `internal/mining/pattern_mining.go`: *"it is the one caller-controlled field (the
  `X-Talyvor-Feature` header), so keying rarity on it would let a workspace manufacture uniqueness
  by varying the header. feature_category is still PERSISTED on the row (analytics) — **it just
  cannot move the score**."*
- `internal/cohort/cohort.go`: *"feature_category is DECLARED at the boundary … **This package
  reaches no ledger/mint path**."*

### What *is* signed is something else

talyvor-track's `internal/lensintegration/webhook.go` HMACs the **webhook payload**, and its own
comment says the issue is matched by the `X-Talyvor-Feature` value *"SCOPED to the workspace the
signed payload names."* **The signature authenticates the workspace. The identifier inside it is
still the caller's string.**

⚠ **AND THAT COMMENT CARRIES THE SAME SUBSTITUTION, IN ANOTHER REPO, STILL LIVE.** It states
*"Lens uses the issue identifier as the X-Talyvor-Feature value"* and calls
`GetByIdentifier(ctx, p.Feature, p.WorkspaceID)`. Migration 0116 above records that premise as false
and as the cause of a shipped defect; 0116's fix landed on the **spend-by-request join**, and the
**alert → notification path was not moved to it**. Measured read-only from talyvor-lens: the value
that reaches `p.Feature` is an alert *rule's* configured feature string (`internal/alerts/alerts.go`
matches `rule.Feature != feature` and emits `rule.Feature`), which for the extension Talyvor ships
is `"code-chat"`. **Not fixed here — that is a talyvor-track merge and this session held
talyvor-lens.** Filed for the queue.

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

- `TestPremise_NoWorkItemHeaderIsSignedOrVerified` — **two-sided, PER HEADER**: each header in the
  census must actually *be read*, so "nothing verifies it" can never be a statement about the
  search — and the floor is per-header rather than a union, so one header's readers cannot cover
  for another's going to zero. **Expected to go red the day a real signature check appears on a
  work-item header** — which is the day premise 1 becomes true and this brief's blocking half can
  be deleted. A verification on the *declared category* reds it too, with a different message that
  says plainly that it does **not** unblock W6.1.4.
- `TestPremise_TheWorkItemHeaderIsTheIssueHeaderNotTheFeatureHeader` — anchors the census's `role`
  column in the product rather than in the test's opinion, by reading the code (the extractor's
  `IssueID` mapping and `recordSQL`'s `issue_id`) rather than a comment. It diagnoses the
  `IssueID`-from-`X-Talyvor-Feature` substitution **first**, deliberately: that mutation also
  removes the `X-Talyvor-Issue` mapping, so checking the general case first made the specific,
  already-shipped-once diagnosis **unreachable** — control X7 measured exactly that.
- `TestPremise_TheRepoAlreadyRefusesToLetTheDeclaredFieldScoreOrMint` — reads the actual text of
  both refusals, independently, so one going stale cannot hide the other.
- `TestPremise_SavingsPctIsStillWriterless`.

Controls: `w614-workitem-header-controls-x9t4.py`, **8/8 as predicted** (superseding
`w614-premise-controls-k7v3.py`, 4/4, which controlled the version that watched the wrong header).

⚠ **X1b is the one to read.** It applies X1's mutation — a real gateway signature check on
`X-Talyvor-Issue` — against the guard **as previously merged**, and it is **NOT CAUGHT**. The guard
passed and logged *"5 files read X-Talyvor-Feature … the work-item id is caller-declared"* over a
tree in which the work-item header was signed. A guard whose population boundary is the wrong header
looks exactly like a guard over a healthy product.

⚠ **X5 is the blindness the per-header floor exists for.** With the only `X-Talyvor-Issue` reader
removed *and* the floor rewritten as a union, **the floor goes silent** — `X-Talyvor-Feature`'s four
reader files satisfy it — while only the role anchor reds.

⚠ **X6 keeps CAUGHT from being a catch-all**: an extra plain `Header.Get`/`Set` of the work-item
header, in a file that did not read it before, is **not caught**. Reading is not verifying.

⚠ **X6's first cut was VOID, not a result**: it added an `IssueID` field to
`internal/attribution.Attribution`, which has no such field, so it did not compile — and a control
that cannot build proves the compiler noticed rather than the test. Re-cut against
`helicone.go`'s own existing forwarding shape.

⚠ **KEPT FROM THE SUPERSEDED HARNESS, because the lesson outlives the guard it controlled:** its K2
mutated the test rather than the product, correctly — the anti-blindness floor is reachable only with
an empty population. K2's *first* version removed one of the readers and left four, never tripped the
floor, and reported MISSED while the floor was working fine — **a control too small to reach the
thing it was checking.** X4/X5 here are the same shape, sized to actually empty a header's
population.

⚠ **The previous harness's `5 files` was itself a miscount**: it appended one entry per *line* and
reported the total as *files*, and `internal/compat/helicone.go` contributes two lines. The honest
figure for `X-Talyvor-Feature` is **four files**. The census now counts distinct files.
