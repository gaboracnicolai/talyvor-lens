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
_(last updated from sync: main at 0ae7872 — DISTILL conversion core shipped)_

### Nicolai + Claude — in progress
- DISTILL feature (building in stages). Conversion core + PDF converter DONE; next: cache + savings (stage 2).

### Nicolai + Claude — up next (the roadmap — ours, don't take)
- DISTILL remaining stages, in order:
  - 2. Conversion cache + measured savings → token_events (NEXT — collision-free; touches cache + metering, not proxy/ledger)
  - 3. Request-path integration (⚠️ touches internal/proxy — seam to watch; carries the resource-isolation STAGE 3 BLOCKER below)
  - 4. Fidelity tiers (faithful/structured/outline) + preview endpoint
  - 5. Vision fallback for scanned PDFs (NeedsVision now real)
  - 6. Dashboard panel + ROI-report line
- token economy Phases 2–5 (⚠️ GATED on a ledger-seam sync — touches the ledger/economy code the collaborator has been in)
- SOC2 foundation
- PoVI minting go-live: NOT a build — see preconditions section below

### Collaborator — recently landed (all merged to main, in our base)
- Pessimistic locking on ledger writes (extended the existing PoVI FOR UPDATE pattern — seam #1 satisfied).
- Global lexicographic lock ordering in Transfer() (#32) — resolved the open lock-ordering review item himself.
- atomic ExecuteTrade (#34), hash partitioning (#21, 0034 + his own #29 fix), security hardening (#28, 0036 + touched our costanomaly/dashboard), rate-limiting (#23), control-plane + Redis routing (#25, 0035), multi-process readiness, PgBouncer, CI/benchmark.
- edge-infra xDS HA — in his comments, NOT yet pushed (edge-infra frozen at 05-20).

### Done (recently merged to main — drops off both lists)
- DISTILL conversion core (#36) + PDF converter (#37, ledongthuc/pdf BSD-3) — HTML/DOCX/XLSX/CSV/JSON/XML/text/PDF converters + golden corpus; NeedsVision real for text-less PDFs.
- Chart audit items (d)+(e) + PgBouncer-safe migrations (#30); migration chain validates 36/36.
- Minor follow-ups (#35): local-routing spend note, anomaly-panel axis labels.
- All earlier audit follow-ups (f)/(g), buffered-output-guardrail fix, cleanup batch.

---

## Open coordination items
- **⚠️ STAGE 3 BLOCKER — enforced resource isolation for untrusted-doc conversion.** PDF (and any untrusted-document) conversion must run under enforced resource isolation — a separate killable process/cgroup with hard memory + CPU + wall-clock limits — before request-path exposure. In-leaf bounds are insufficient: a zlib-bomb PDF can OOM and a cyclic-ref PDF can hang/stack-overflow inside ledongthuc, and Go cannot catch OOM/stack-overflow with recover() nor kill a runaway goroutine. The 10 MiB input cap + recover handle the catchable failures only. **Resource isolation is arguably infra-tier — a candidate coordination item with the collaborator (who proposed process isolation).** Captured in code at pdf.go + skipped tripwire test TestPDFResourceResidual_KNOWN.
- **DISTILL request-path integration (stage 3, upcoming)** — will touch internal/proxy. Collision-free now, but sync before building it if the collaborator starts on the proxy/request path. (Carries the resource-isolation gate above.)
- **Token economy Phases 2–5 (gated)** — touches the ledger/economy code the collaborator actively worked. MUST sync on seam #1 before starting.
- **`costanomaly` / `dashboard` seam** — his #28 touched these (our app-tier); check what it changed before next editing those files.
- **PgBouncer / migrations seam (handled)** — migrate Job takes explicit migrations.databaseURL to go direct; his pooler untouched.
- **Ledger lock ordering (resolved by him, #32)** — global lexicographic ordering added; no longer open.

---

## PoVI minting — preconditions before go-live
The minting mechanism is BUILT and on main, shipped OFF by default (`LENS_POVI_MINTING_ENABLED=false`; trust-mint retirement switch also not flipped). Flipping it on is an operator/business decision, NOT a build milestone. **Leaving it off costs nothing and carries zero risk — the un-flipped switch is correct, not unfinished.** Preconditions before enabling, all of which are mostly NOT engineering tasks:
1. **A real node network exists** — minting rewards nodes for serving inference; with zero independent operators it mints into a vacuum. Downstream of users/demand (the same constraint as business success).
2. **Security model survived something real** — live concurrent testing (not just pgxmock) + ideally a qualified external security/crypto audit of PoVI. Minting on an unaudited novel economic-security mechanism is the highest-risk action in the codebase.
3. **Token economy complete** — Phases 2–5 built, not just the Phase-1 security model.
4. **A deliberate legal/economic decision on what LENS is** — a mintable value-bearing token has regulatory dimensions (securities/money-transmission, jurisdiction-dependent). Not a code decision.
5. **Enable in a controlled/testnet env first**, then load-bearing.

The trigger is not another build — it's a real network + an audit + a legal call, all downstream of the customer question. (Full note: povi-minting-preconditions.md.)
