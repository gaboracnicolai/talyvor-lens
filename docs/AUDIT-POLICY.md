# Audit-Trail Integrity & Retention Policy (U14)

This document records Talyvor Lens's audit-trail integrity guarantees, the threat
model they hold under, retention defaults, and the off-box export mechanism. It is
intended to be defensible in an enterprise security review.

## 1. The trail inventory

### Append-only set (enforced immutable — migration 0055)
| Table | What it records | Partitioned |
|---|---|---|
| `token_events` | one row per AI call (spend, tokens, model, PII flag) | HASH(workspace_id), 8-way |
| `lens_token_ledger` | every LENS credit/debit (the money ledger) | HASH(workspace_id), 8-way |
| `lxc_ledger` | every LXC credit/debit (fiat usage credit ledger) | HASH(workspace_id), 8-way |
| `request_attribution` | per-request spend attribution (branch/PR/author) | no |
| `povi_receipts` | signed proof-of-inference receipts (Merkle root + verify) | no |

### Mutable-by-design set (deliberately NOT guarded — legitimate state transitions)
| Table | Why it must mutate |
|---|---|
| `lxc_purchases` | a refund marks `status='refunded', refunded_at` (`internal/billing`) |
| `pool_royalty_mints` | the holdback lifecycle CAS: `held → final / revoked` (`internal/poolroyalty`) |
| `audit_export_state` | the export watermark advances on every successful export (0056) |

## 2. Enforcement (migration 0055)

A single trigger function (`audit_block_mutation`) is attached to the five
append-only tables:

- **`BEFORE UPDATE OR DELETE … FOR EACH ROW`** on each parent. A row trigger on a
  partitioned parent is **automatically created on every partition** (current and
  future), so `UPDATE`/`DELETE` are rejected whether issued against the parent OR
  directly against a partition (empirically verified on pg16).
- **`BEFORE TRUNCATE … FOR EACH STATEMENT`** on each parent **and every partition**.
  TRUNCATE triggers do **not** cascade to partitions, so they are enumerated; the
  hash modulus is fixed at 8, so the partition set is closed.

### Scoped retention bypass
`token_events` is both append-only **and** the retention target, so a `DELETE` on
`token_events` is permitted **only** inside a transaction that runs
`SET LOCAL lens.audit_retention = 'on'` — which only the retention sweeper
(`internal/audit/retention.go`) does. `UPDATE` and `TRUNCATE` remain blocked even
under the flag; the **ledgers are never deletable** (see §4). A stray `DELETE`
without the flag (app bug / injection) is still rejected.

**The append-only guarantee has exactly one scoped exception:** the retention
sweeper may `DELETE` aged `token_events` rows under the flag; `UPDATE`/`TRUNCATE`
and all ledger tables (`lens_token_ledger`, `lxc_ledger`) have **no exception**.
This single-caller property is pinned by a static test
(`TestAuditIntegrity_RetentionBypassFlagSingleCaller`): the flag is referenced in
exactly one non-test file — the sweeper — and any second reference fails the build.

### ⚠ Repartitioning constraint
The `BEFORE TRUNCATE` triggers were enumerated over the partitions that existed
**at migration time**. If any future migration repartitions `token_events`,
`lens_token_ledger`, or `lxc_ledger` (new partitions, a different modulus, or
range instead of hash), the `audit_no_truncate` triggers **must be re-applied** to
the new partition set. The row-level `audit_no_mutation` trigger re-cascades
automatically; the TRUNCATE triggers do not.

## 3. Threat model — stated honestly

**Protects against:** application bugs, accidental mutation, and SQL-injection that
issues `UPDATE`/`DELETE`/`TRUNCATE` against the audit tables (via the parent or a
partition).

**Does NOT protect against:** a **compromised database role**. The role that runs
migrations is the same role the application runs as, and it can
`ALTER TABLE … DISABLE TRIGGER`, `DROP TRIGGER`, or `SET lens.audit_retention='on'`
to bypass the guard. Closing this requires **DB role separation** (a migration-only
role that owns the triggers; the app role with `UPDATE`/`DELETE`/`TRUNCATE` revoked)
— tracked in the U14 follow-up issue *"DB role separation"*. Tamper-evidence
(hash-chaining / signed checkpoints) is a separate, additive layer not in this PR.

## 4. Retention

- **Config:** `LENS_AUDIT_RETENTION` (Go duration). **Default unset / `<= 0` =
  disabled** (rows kept indefinitely). The sweeper runs leader-only, hourly,
  deleting `token_events` older than the window in batches of **5000** to bound
  lock/WAL/vacuum cost on the highest-volume table.
- **`token_events` ONLY.** The sweeper is **hardcoded** to `token_events` — there
  is no table knob, so a config can never point it at a ledger.
- **The ledger constraint (why ledgers are never retention-eligible):**
  `mining.LedgerStore.GetTotalSupply` (`internal/mining/cache_mining.go`) computes
  total LENS supply as `SUM(amount)` over the **entire** `lens_token_ledger`
  history, feeding the peg and circulating supply. Deleting old ledger rows would
  silently corrupt reconciliation. A static test
  (`TestAuditIntegrity_NoLedgerMutationInProductionCode`) pins **zero**
  `UPDATE`/`DELETE` against `lens_token_ledger`/`lxc_ledger` in production code,
  forever.

## 5. Off-box export

- **Config:** `LENS_AUDIT_EXPORT_URL` (sink) + `LENS_AUDIT_EXPORT_INTERVAL`
  (Go duration, default 1h). **Default off** (empty URL ⇒ no loop).
- A leader-elected loop exports `token_events` in the window `(watermark, now]` as
  NDJSON to the sink (reusing the audit webhook exporter), advancing the persisted
  watermark (`audit_export_state`, 0056) to `now` **only on a successful POST**. On
  failure the watermark does not advance, so the next run re-covers the gap:
  **at-least-once** delivery (no audit row is lost; a boundary row may rarely
  duplicate — dedup on `request_id`).

### Retention ↔ export ordering
Retention and export are **independent** loops. By **default** the retention sweeper
deletes by age and does **not** consult the export watermark (today's behaviour,
unchanged), so if `LENS_AUDIT_RETENTION` is shorter than the export lag, retention
could delete `token_events` rows before they are exported. **Operational guidance for
the default:** set `LENS_AUDIT_RETENTION` far larger than the export interval
(months/years vs. minutes/hours).

**Proof-of-export-before-delete (U14 #187, opt-in).** Set
`LENS_AUDIT_REQUIRE_EXPORT_BEFORE_PRUNE=true` to gate the sweeper behind the export
watermark: a row is pruned only when `created_at <= audit_export_state.last_exported_at`
**and** `< (now − retention)`, so an aged-but-un-exported row is **kept** until it has
been exported off-box. The watermark is read once per sweep (it only advances, so a
stale read prunes less, never more). If export is **disabled** while this is on, the
sweep is **skipped with a warning** rather than pruning un-exportable rows. Default
`false` preserves the age-only behaviour exactly — flipping it on is the
customer-/compliance-driven decision, not a code default.

## Follow-ups
- **DB role separation** (migration role vs app role) — for DB-enforced immutability
  against a compromised role. Tracked: #186.
- **Export-then-prune watermark** — proof-of-export-before-delete. BUILT (#187,
  opt-in via `LENS_AUDIT_REQUIRE_EXPORT_BEFORE_PRUNE`, default off).
