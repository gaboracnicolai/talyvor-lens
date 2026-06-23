# Distill reuse-royalty — TEST/STAGING turn-on runbook

> # ⚠️ TEST / STAGING ONLY
> Following this runbook flips on the distill reuse-royalty mint, which moves
> **internal LENS ledger value to test accounts. It is NOT real money** — LENS is a
> closed-loop internal credit that never converts back to fiat. **Do NOT run this
> against a production/customer deployment.**
>
> The **public / real-money go-live is a SEPARATE decision**, still gated on, and
> NOT covered here: the deferred distill-specific fraud **detector + per-pair caps
> + revoke/adjudication wiring**; the **external crypto/ledger audit**; the legal
> definition of "what is LENS"; and **live Stripe keys** (live billing is blocked
> by the missing UK entity + bank regardless).

Every variable name, default, and command below is confirmed against the code at
main `302dc48` (citations inline). The mint is built **inert** — these steps turn it
on **in a throwaway test deployment**.

---

## 1. Env flags (TEST config)

| Env var | Set to | Default | What it does |
|---|---|---|---|
| `LENS_ECONOMY_ENABLED` | *(leave unset)* | **`true`** (explicit opt-out; `config.go:384`, `:1094`) | Master economy kill-switch. **Already on by default** — do NOT set it `false` (that force-offs every mint, incl. this one, `config.go:1098-1102`). Confirm it is not set to `false` anywhere in the test env. |
| `LENS_DISTILL_POOLABLE_ENABLED` | `true` | `false` (`config.go:609`) | The cross-tenant distill **pool substrate**. Off ⇒ no cross-tenant OCR serve ⇒ no basis ⇒ no distill royalty, regardless of the mint flag. |
| `LENS_POOL_ROYALTY_MINTING_ENABLED` | `true` | `false` (`config.go:613`) | The **mint** itself. The distill mint sweeper no-ops before any DB access while this is `false`. (Shared with the cache royalty — that's intentional; the substrate flag above is what independently gates distill.) |
| `LENS_POOL_ROYALTY_SHARE` | *(optional)* e.g. `0.5` | **`0.5`** (`config.go:312`, `:904`), clamped to `[0,1]` | Contributor share `s`: minted `= s × avoided_cogs_usd`. The Burn-and-Mint invariant needs `s ≤ 1` (net `(1−s)×COGS ≥ 0`). 0.5 is the grounded default; settable. |
| `LENS_POOL_HOLDBACK_WINDOW` | *(staging)* e.g. `30s` | **`72h`** (Go duration; `config.go:845-851`) | Held→final settlement delay. **Set short in staging** (e.g. `30s`) so you can watch finalize without waiting 72h. |

Minimal staging env:
```sh
# LENS_ECONOMY_ENABLED defaults true — just don't set it false.
export LENS_DISTILL_POOLABLE_ENABLED=true
export LENS_POOL_ROYALTY_MINTING_ENABLED=true
export LENS_POOL_HOLDBACK_WINDOW=30s     # staging only; production stays 72h
# export LENS_POOL_ROYALTY_SHARE=0.5     # optional; this is already the default
```

---

## 2. Test-workspace setup (two steps — the flag alone does nothing)

Assume owner **`wsA`** (the OCR contributor) and requester **`wsB`** (the reuser).

### 2a. Dual consent — `distill_poolable=true` on BOTH A and B

A cross-tenant OCR serve requires the global flag **AND** both workspaces opted in
(`cache_pooling.PoolabilityGate.MaybeAllowPooledHit` — owner + requester checked).
The per-workspace opt-in is the `workspaces.distill_poolable` column (migration
0051), set via the admin API (preferred — updates in-memory + DB immediately,
`internal/workspace/distill_poolable.go:41`):

```sh
# API (immediate). Repeat for BOTH wsA and wsB.
curl -X PUT "$LENS_URL/v1/workspaces/wsA/distill-poolable" \
  -H "Authorization: Bearer $ADMIN_KEY" -H 'Content-Type: application/json' \
  -d '{"distill_poolable": true}'                       # main.go:2754-2757
curl -X PUT "$LENS_URL/v1/workspaces/wsB/distill-poolable" \
  -H "Authorization: Bearer $ADMIN_KEY" -H 'Content-Type: application/json' \
  -d '{"distill_poolable": true}'
```

DB fallback (needs an in-memory reload — bounded by `WorkspaceReloadInterval` — to
take effect on the serve path, so the API above is preferred):
```sql
UPDATE workspaces SET distill_poolable = true WHERE id IN ('wsA','wsB');
```

### 2b. Clear the U6 verified-floor for the OWNER (A) so it can earn

`earnverify.MayEarn(A)` (the Sybil floor) requires A to be **either** admin-vouched
**or** to have a completed real-money LXC purchase (`internal/earnverify/verify.go:27-29`):
```sql
EXISTS(SELECT 1 FROM workspaces      WHERE id = 'wsA' AND earn_verified = true)
OR EXISTS(SELECT 1 FROM lxc_purchases WHERE workspace_id = 'wsA' AND status = 'completed' AND lxc_amount > 0)
```

> **WITHOUT this, the floor correctly pays A ZERO** — the distill mint's
> `CreditHeldTx` hits `verifyEarn(A) → ErrEarnNotVerified`, the whole tx rolls back,
> and the relationship stays un-minted (re-eligible once A verifies). **That is the
> Sybil-zero behavior, not a bug.**

Use EITHER path (both make `MayEarn('wsA')` true):

**Admin-vouch (DB-only — there is intentionally NO self-serve API; a workspace must
not be able to self-vouch):**
```sql
UPDATE workspaces SET earn_verified = true WHERE id = 'wsA';   -- migration 0057
```

**Test LXC purchase (record a completed purchase directly):**
```sql
-- columns confirmed against migrations/0054_lxc_purchases.sql (stripe_event_id UNIQUE NOT NULL,
-- workspace_id NOT NULL, usd_cents NOT NULL; status defaults 'completed', lxc_amount must be > 0)
INSERT INTO lxc_purchases (stripe_event_id, workspace_id, usd_cents, lxc_amount, status)
VALUES ('test_evt_wsA_1', 'wsA', 100, 10, 'completed');
```

(Refunded/anomalous purchases are deliberately excluded by the floor — they don't verify.)

---

## 3. Stripe / billing — OPTIONAL, only to test the LXC-purchase FLOW

**Not needed for the royalty itself** — §2b's `earn_verified` or the direct
`lxc_purchases` insert clears the floor without any Stripe traffic. Set these ONLY
if you want to exercise the real Stripe checkout → webhook → `lxc_purchases` path:

| Env var | Value | Notes |
|---|---|---|
| `LENS_BILLING_ENABLED` | `true` | Default `false` (`config.go:392`). When `true`, BOTH keys below are required or boot fails (`config.go:697-698`). |
| `LENS_STRIPE_SECRET_KEY` | `sk_test_…` | `config.go:627`. |
| `LENS_STRIPE_WEBHOOK_SECRET` | `whsec_…` | `config.go:628`. |

> **TEST keys ONLY (`sk_test_…`), NEVER live (`sk_live_…`).** Live billing is blocked
> by the missing UK entity + bank regardless; do not attempt it.

---

## 4. Verification recipe

1. **A produces**: send a request to **wsA** carrying a scanned (image-only) PDF with
   distillation on (`X-Talyvor-Distill: true` or the workspace's distill policy). A
   OCRs it; with `distill_poolable=true` the OCR is published to the shared pool.
2. **B reuses**: send the **same scanned bytes** to **wsB**. B is served A's pooled
   OCR cross-tenant (consented) → PR2 writes the **basis** row.
3. **Mint** (within ~1 min — the leader-elected mint sweeper ticks each minute): a
   **held** row appears in `distill_royalty_mints` for A.
4. **Finalize** (after `LENS_POOL_HOLDBACK_WINDOW`): the distill finalize sweeper
   settles it → `status='final'`, held→spendable, supply increments.

### The literal SQL (against the test DB)

```sql
-- (1) BASIS recorded (the pinned avoided-COGS snapshot, PR2)
SELECT owner_workspace_id, requester_workspace_id, content_hash, avoided_cogs_usd
FROM distill_royalty_basis
WHERE owner_workspace_id = 'wsA' AND requester_workspace_id = 'wsB';

-- (2) HELD mint to A == s × basis (s = 0.5), status 'held'
SELECT m.contributor_workspace_id, m.minted_amount, b.avoided_cogs_usd,
       (m.minted_amount = 0.5 * b.avoided_cogs_usd) AS amount_is_exact,
       m.status, m.request_id
FROM distill_royalty_mints m
JOIN distill_royalty_basis b
  ON b.owner_workspace_id = m.contributor_workspace_id
 AND b.requester_workspace_id = m.requester_workspace_id
 AND b.content_hash = m.content_hash
WHERE m.contributor_workspace_id = 'wsA';
-- expect: minted_amount = 0.5 × avoided_cogs_usd, amount_is_exact = t, status = 'held'

-- (3) After the holdback → FINAL + supply increments
--     status flips to 'final'; held→spendable on A's balance; and the COUNTED
--     'pool_royalty' ledger row (the supply moment) is written for A.
SELECT status FROM distill_royalty_mints WHERE contributor_workspace_id = 'wsA';     -- 'final'
SELECT balance, held_balance FROM lens_token_balances WHERE workspace_id = 'wsA';    -- held→balance
SELECT amount, type FROM lens_token_ledger
WHERE workspace_id = 'wsA' AND type = 'pool_royalty';                                -- the supply row
-- (the HELD credit was type 'pool_royalty_held' — UNCOUNTED; supply counts only the
--  'pool_royalty' row written at finalize)
```

Run the one-shot checker instead of eyeballing:
```sh
LENS_TEST_DATABASE_URL=postgres://… ./scripts/verify-staging-economy.sh wsA wsB
```

---

## 5. What this does NOT turn on (the real-money go-live gates)

Turning on the staging economy proves the mechanism end-to-end with internal test
value. The **public, real-money flip remains separately gated** on all of:

- the deferred **distill-specific fraud detector + per-pair/per-entry caps + revoke/
  adjudication wiring** (PR3 reused the shared finalize kernel but deliberately did
  NOT add distill detection — the U6 floor + per-identity rate cap + card-fingerprint
  linkage are the closed test meanwhile);
- the **external security/crypto + ledger audit** of the mint/ledger path;
- the **legal definition of LENS** (closed-loop internal credit, never fiat-convertible);
- **live Stripe keys** + a real billing entity (blocked by the missing UK entity + bank).
