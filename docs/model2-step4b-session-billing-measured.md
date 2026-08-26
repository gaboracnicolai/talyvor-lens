# W4.6.1 step 4b — measured before it is built: a session-key request bills nothing

Step 4b is "the per-session spend bound". Step 4's own handover recorded why it was deferred:

> **PER-SESSION SPEND BOUND: NOT BUILT, AND THE COLUMN IS ABSENT RATHER THAN PRESENT-AND-UNREAD** —
> a column nothing reads is a claim that a bound exists when it does not. Wiring one changes the LXC
> admission gate = the SERVING path.

**That understates it, and this document is the measurement.** A session key does not merely lack a
*bound*. In the default configuration a session-key request **moves no LXC at all** — no hold, no
settle, no shadow debit. There is nothing for a bound to bound, and nothing that bills the customer.

## What was measured

`internal/proxy/session_key_billing_realpg_test.go`, real Postgres, through the real handler, on
**both seams**. The same request, twice, differing only in the credential:

| credential | upstream calls | `lxc_ledger` rows | `lxc_reservations` rows | balance |
| --- | --- | --- | --- | --- |
| **session key** (`APIKeyID` empty) | 1 | **0** | **0** | **untouched** |
| workspace key (`APIKeyID: "agent-1"`) | 1 | > 0 | 1, `settled` | −260,000 µLXC |

The session-key `AuthContext` is **built by the real `auth.Manager.Authenticate`**, not hand-written
— a fixture asserting its own `APIKeyID: ""` would prove nothing about `internal/auth`.

## Why — and it is one branch

`serve()`'s **entire** LXC admission-and-debit block is inside `if agentKeyID != ""`, and
`agentKeyID` is `AuthContext.APIKeyID`. `internal/auth` sets `APIKeyID` on the **workspace-key branch
and nowhere else**, deliberately. `internal/auth`'s own test says why:

> *"it keys the F4 per-agent LXC sub-budget allocator; a session key is not a scoped workspace key
> and must not enter that path"*

**That refusal is right on its own terms** — a credential that dies in an hour should not open a
sub-budget, and migration 0122 argues it at length. What nobody wrote down is that on the serve path
the allocator is the **only** thing that moves LXC by default. So *"must not enter the sub-budget
path"* and *"is never billed"* are **the same branch**.

### The other two movers are off by default

A census of every LXC value movement in `internal/proxy` finds exactly three:

1. `SpendLXCForAgent` — inside `agentAllocationBlocks`, requires a non-empty `APIKeyID`.
2. `ReserveLXCForAgent` / `SettleLXCReservation` — the hold returns early on an empty `APIKeyID`, and
   `settleReservation`'s own docstring says it returns 0 "when there is NO reservation — a
   non-agent/plain-key request".
3. `shadowSpendLXC` → `SpendLXC` — gated on `LXCShadowSpendEnabled`, which `config.Load` leaves
   **false** (`parseBoolEnv` with no default), and whose call sites sit in the `else` of
   `reservationActive()`, whose two flags both default **true**.

Control **Y2** confirms (3) is a real escape hatch rather than a dead branch: with the shadow sink
wired and the reservation path off, the session key **does** bill. The finding is about the default
configuration, not a structural impossibility.

## Why no existing test could see it

**The fixtures are uniform.** Every serve-path billing test in `internal/proxy` stamps a *non-empty*
`APIKeyID` (`"agent-1"`, `"agent-"+ws`). The whole billing population is agent traffic, so no
assertion has ever asked what a proxy request *without* an `APIKeyID` bills. It is the same shape as
the tier dot that drew "cheap" for every model outside a two-entry map: **a default cannot be told
from a hit when every subject is a hit.**

## Scope, stated rather than implied

- Session-key **routes are off by default** (`LENS_SESSION_KEYS_ENABLED=false` ⇒ never registered,
  every `tlv_sk_` bearer refused). This is **latent on a default deployment, not live.** It becomes
  live the moment step 4 is switched on — which is what step 6 (the chat screen) needs.
- **Not measured here:** whether the request is still recorded in `token_events`. `costWireProxy`'s
  fixture does not create that table at all (probed, not assumed). So *"metered but not billed"* is
  only half-measured — the **not billed** half. Do not repeat the other half as if this file proved it.
- The global admin key also carries an empty `APIKeyID` and is therefore also unaccounted. That is an
  operator credential, not a customer one, and is out of scope here.

## Why nothing is fixed

Which accounting a session key should get is a **decision**, and each option costs something:

1. **Give it a reservation identity** (key the hold on the session-key id). Directly contradicts
   migration 0122's documented refusal — *"a credential that dies in an hour must not open a
   sub-budget"* — and `AuthContext.APIKeyID` is the field the allocator reads, so this means either
   widening that field's meaning or adding a second one.
2. **Debit the workspace directly at settle.** That is the shadow path, which is off by default for
   its own reasons (it is a no-reservation, no-refund immediate debit).
3. **A self-accounting per-session bound** — a `spend_bound_ulxc` on `session_keys` plus its own
   accumulator, checked pre-serve and added post-serve, both no-ops when the bound is NULL. This is
   step 4b as literally written. It caps the **provider bill** Talyvor pays for a runaway chat, which
   is real value — but it still **bills no customer**, so shipping it alone would leave the ledger
   exactly as silent as it is today while looking like the money question had been answered.

**(3) is buildable without a decision and (1) and (2) are not.** But (3) should not ship described as
"the session spend bound is now enforced" unless this document's finding ships with it, or the next
reader will believe a session-key request is accounted for when it is not.

## Controls

`~/talyvor-queue/w461-sessionbilling-controls-x9t4.py` — **6/6 as predicted**. It **refuses to run**
without `LENS_TEST_DATABASE_URL` rather than scoring itself green over skipped tests.

| | mutation | result |
| --- | --- | --- |
| Y0 | baseline | GREEN |
| **Y1** | `internal/auth` gives the session credential an `APIKeyID` (**product**) | CAUGHT — the zeros are that branch |
| Y2 | shadow debit ON + reservation OFF (**configuration**) | CAUGHT — the session key then bills |
| Y3 | both arms share one prompt again | CAUGHT by the anti-blindness arm |
| Y4 | the ledger reader points at a different workspace | CAUGHT by the anti-blindness arm |
| Y5 | the session key's `UserID` changed | NOT CAUGHT (catch-all guard) |

⚠ **The anti-blindness arm caught a real defect in the harness before any of this was written.** The
first version sent a **byte-identical body from both arms**, so the second arm was a **cache hit**: it
never called upstream, released its hold and billed nothing. The measurement reported *"THE HARNESS
CANNOT SEE A BILL"* and refused. **Arm 1's zeros had been meaningless** — two arms that share a cache
are one arm run twice. Y3 is that failure, kept as a control.

⚠ **The upstream-call counter is not decoration either.** Without it, *"nothing was billed"* and
*"nothing happened"* are the same observation: a cached or refused request also bills nothing and
would satisfy every zero-assertion. Both arms now assert **exactly one** upstream call.
