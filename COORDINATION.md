# Talyvor — Work Coordination

**Purpose:** two people build in these repos in parallel, each running Claude / Claude Code. Our Claudes share no memory and cannot see each other's work — **GitHub is the only place our work meets, so GitHub is the single source of truth.** This file is how we avoid double work and handle the seams where our work touches.

**Last synced:** _(update at each session start)_

---

## Who owns what

This is NOT a 50/50 split of the roadmap. The roadmap is owned by Nicolai.

### Nicolai (with Claude) — the planned roadmap, end to end
Everything in the Talyvor master plan and everything Nicolai + Claude have scoped: the Lens gateway and all its tiers (hardening, moat, parity), the token economy (PoVI Phases 1–5), the application/feature layer, DISTILL, the follow-up list, SOC2 foundation, the sibling products (Track / Docs / Code) when their turn comes. **This is the primary work. It is not divided with the collaborator.** The collaborator does not take items off this plan.

### Collaborator — additive infra/data-tier work, his own initiative
Polishing and hardening from his own expertise, *extra* to the planned roadmap, not carved out of it:
- Data tier: table partitioning (`lxc_ledger`, `token_events`), connection pooling (PgBouncer).
- Concurrency: pessimistic locking on DB writes, consistent global lock ordering.
- Resilience: chaos testing, process isolation.
- edge-infra: xDS control-plane HA (heartbeat reuse and/or externalized state).

He works on his own copy/branches and merges to main once approved.

---

## The seams — where our work touches (handle deliberately)

Most of the time we're in different code and won't collide. Collisions happen at **seams** — places both sides legitimately touch. Known seams:

1. **The token ledger (the big one).** It is *data tier* (his locking/partitioning) AND *token-economy logic* (our PoVI work, which already has `FOR UPDATE` stake operations: `LockStake` / `ReleaseStake` / `SlashStake`). **Rule: any new locking must EXTEND the existing PoVI `FOR UPDATE` pattern with ONE global lock ordering — never add a second locking discipline alongside it.** This is money-handling code; two locking patterns is how you get deadlocks and lost writes.

2. **Migrations.** Both sides add migration files. **Rule: never edit the other person's migration. If you find a bug in theirs, hand it back with a precise report — don't silently fix it on your branch (that creates two diverging copies of the same migration).** (This already happened once: the migrate runner surfaced a fresh-apply bug in the collaborator's `0034` partitioning migration — handed back, not patched.)

3. **`cmd/lens/main.go` entrypoint.** Both sides may touch it (we added a `migrate` subcommand; infra work may add flags). **Rule: small, additive changes only; the default server-start path must never change behavior.**

---

## The rules (short version)

1. **GitHub is truth.** Not anyone's Claude's memory, not a notes file. Before building, check what's actually on `origin/main`.
2. **Never push to the other person's branch.** Work crosses between us only as reviewed → merged PRs.
3. **Never edit the other person's migration / recent work.** Find a bug? Hand it back with a precise report.
4. **Session starts with a status sync** (see the standard prompt — run it every time).
5. **Small, frequent, single-purpose PRs.** Short-lived branches. The enemy is a giant branch that has to reconcile against a hundred of the other person's commits.
6. **At a seam, coordinate explicitly** — especially the ledger.

---

## Work ledger — keep current
_(last updated from sync: main at 1d8d150)_

### Nicolai + Claude — in progress
- Chart migrate-runner + backup CronJob — branch `feat/lens-chart-migrate-and-backup` (WIP commit: runner + `lens migrate` subcommand + 0022 repair done; chart hook + CronJob in progress).

### Nicolai + Claude — up next (the roadmap — ours, don't take)
- Finish chart pieces (migrate hook + backup CronJob)
- DISTILL · token economy Phases 2–5 · SOC2 foundation
- Remaining follow-ups: local-routing spend note, anomaly-panel UX polish (b)

### Collaborator — recently landed (all merged to main, in our base)
- Pessimistic locking on ledger writes (`8a3ca27`): implicit ON CONFLICT → explicit SELECT FOR UPDATE, atomicized StakeManager (`internal/povi/stakes.go`, `internal/mining/stake_ledger.go`, `internal/economy/dualtoken.go`). **Extended the existing PoVI FOR UPDATE pattern — seam #1 satisfied.**
- Hash partitioning (`5f0002c` / migration `0034`) — **has a fresh-apply bug, handed back (see below).**
- Rate-limiting public endpoints (#23 / `45e28d5`).
- Control-plane node reconciler + Redis routing (#25) + migration `0035_controlplane.sql`.
- Multi-process readiness (leader election, PG read-through), CI workflow, benchmark fix.
- edge-infra xDS HA — discussed in his comments, NOT yet pushed (edge-infra frozen at 05-20).

### Done (recently merged to main — drops off both lists)
- All audit follow-ups (f)/(g), buffered-output-guardrail fix, cleanup batch — ours, merged.

---

## Open coordination items
- **`0034` partitioning migration (his, in our base)** — fresh-apply bug: 7 indexes collide because CREATE INDEX runs before the DROP TABLE that frees the renamed-table index names. Fix shape (his to apply in his own branch): per block, move CREATE INDEX to after DROP TABLE; do NOT use IF NOT EXISTS (would skip and leave the partitioned table unindexed). Our migrate runner is validated against `0001–0033`, correctly fails loud at 0034.
- **`0035_controlplane.sql` (his, in our base)** — sits AFTER the broken 0034, so our fresh-apply e2e never reached it; it's currently unvalidated. Fixing 0034 unblocks validation of BOTH 0034 and 0035 → full 35/35 with zero changes on our side.
- **Ledger lock ordering** — his pessimistic locking extended the PoVI FOR UPDATE pattern (good). Remaining review item (not a conflict): confirm global lock ordering is consistent across all ledger tables to prevent deadlocks.
