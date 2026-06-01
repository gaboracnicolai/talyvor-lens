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
_(last updated from sync: main at 347a916 — chart work merged, migrations 36/36 validated)_

### Nicolai + Claude — in progress
- _(none — chart work shipped; next: minor follow-ups, then DISTILL)_

### Nicolai + Claude — up next (the roadmap — ours, don't take)
- Two minor follow-ups: local-routing spend note, anomaly-panel UX polish (b)
- DISTILL (designed, ready to build — self-contained, collision-free with infra work)
- token economy Phases 2–5
- SOC2 foundation

### Collaborator — recently landed (all merged to main, in our base)
- Pessimistic locking on ledger writes (`8a3ca27`): explicit SELECT FOR UPDATE, atomicized StakeManager. **Extended the existing PoVI FOR UPDATE pattern — seam #1 satisfied.**
- Hash partitioning migration `0034` (#21) — had a fresh-apply bug; **HE FIXED IT himself (#29 / `26b45c8`, CREATE INDEX after DROP TABLE).**
- Rate-limiting public endpoints (#23).
- Control-plane node reconciler + Redis routing (#25) + migration `0035_controlplane.sql`.
- Security hardening (#28) — migration `0036_check_constraints.sql` + **touched `internal/{attribution,costanomaly,dashboard}` (costanomaly + dashboard are OUR app-tier code — see seam note below).**
- Multi-process readiness (leader election, PG read-through), PgBouncer (pooling), CI workflow, benchmark fix.
- edge-infra xDS HA — in his comments, NOT yet pushed (edge-infra frozen at 05-20).

### Done (recently merged to main — drops off both lists)
- Chart audit items (d) functional migrate hook + (e) backup CronJob + PgBouncer-safe migrations (#30, merge commit `347a916`).
- Migration chain now validates **36/36 end-to-end** (0001–0036) through our runner, unchanged — his 0034 fix unblocked 0035 + 0036.
- All earlier audit follow-ups (f)/(g), buffered-output-guardrail fix, cleanup batch — ours, merged.

---

## Open coordination items
- **Migrations RESOLVED** — full chain 0001–0036 applies clean (his 0034 fix landed). No outstanding migration issues. Count: 36, no 0037+.
- **New seam to watch — `costanomaly` / `dashboard`:** his security-hardening (#28) touched `internal/costanomaly` and `internal/dashboard`, which are OUR app-tier code. Merged + green, no conflict — but next time we touch those, check what #28 changed there first. The "his infra / our app" split isn't absolute; he reaches into app-tier for cross-cutting hardening.
- **Ledger lock ordering** — his pessimistic locking extended the PoVI FOR UPDATE pattern (good). Remaining review item (not a conflict): confirm global lock ordering is consistent across all ledger tables to prevent deadlocks.
- **PgBouncer / migrations seam (handled):** DDL migrations break through a transaction pooler, so our migrate Job takes an explicit `migrations.databaseURL` to go direct when his PgBouncer is enabled. His pooler untouched; the link is one explicit operator-set value. He should know the seam exists.
