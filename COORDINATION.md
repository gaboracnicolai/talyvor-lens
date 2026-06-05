# Talyvor — Work Coordination

**Purpose:** two people build in these repos in parallel, each running Claude / Claude Code. Our Claudes share no memory and cannot see each other's work — **GitHub is the only place our work meets, so GitHub is the single source of truth.** This file is how we avoid double work and handle the seams where our work touches.

**Last synced:** 2026-06-05 — main at afbe241 (DISTILL 100% complete — stage 3 PRs #0–#4 shipped: #50/#51/#52/#83/#85/#86)

---

## Who owns what

This is NOT a 50/50 split of the roadmap. The roadmap is owned by Nicolai.

### Nicolai (with Claude) — the planned roadmap, end to end
Everything in the Talyvor master plan and everything Nicolai + Claude have scoped: the Lens gateway and all its tiers (hardening, moat, parity), the token economy (PoVI Phases 1–5), the application/feature layer, DISTILL, the follow-up list, SOC2 foundation, the sibling products (Track / Docs / Code) when their turn comes. **This is the primary work. It is not divided with the collaborator.** The collaborator does not take items off this plan.

### Collaborator (Andrei) — additive infra/data-tier work + ISO 27001 security hardening
Two tracks, both additive to the roadmap rather than carved out of it:

**Original scope — own initiative:**
- Data tier: table partitioning (`lxc_ledger`, `token_events`), connection pooling (PgBouncer).
- Concurrency: pessimistic locking on DB writes, consistent global lock ordering.
- Resilience: chaos testing, process isolation.
- Edge-infra: xDS control-plane HA (heartbeat reuse and/or externalized state).

**Expanded scope — security hardening to ISO 27001, coordinated directly with Nicolai:**
- Transport security: TLS enforcement across all layers (HTTPS, Postgres, Redis, nodes).
- Application security: XSS, auth hardening, HTTP security headers, access control gaps.
- Operations security: process isolation hardening, DB atomicity, log injection prevention.
- Does not take roadmap features — only hardens what exists.

He works on his own copy/branches and merges to main once approved.

---

## The seams — where our work touches (handle deliberately)

Most of the time we're in different code and won't collide. Collisions happen at **seams** — places both sides legitimately touch. Known seams:

1. **The token ledger (the big one).** It is *data tier* (his locking/partitioning) AND *token-economy logic* (our PoVI work, which already has `FOR UPDATE` stake operations: `LockStake` / `ReleaseStake` / `SlashStake`). **Rule: any new locking must EXTEND the existing PoVI `FOR UPDATE` pattern with ONE global lock ordering — never add a second locking discipline alongside it.** This is money-handling code; two locking patterns is how you get deadlocks and lost writes.

2. **Migrations.** Both sides add migration files. **Rule: never edit the other person's migration. If you find a bug in theirs, hand it back with a precise report — don't silently fix it on your branch (that creates two diverging copies of the same migration).** (This already happened once: the migrate runner surfaced a fresh-apply bug in the collaborator's `0034` partitioning migration — handed back, not patched. Collaborator has since fixed his own 0034.)

3. **`cmd/lens/main.go` entrypoint.** Both sides may touch it (we added a `migrate` subcommand; infra/security work adds flags and endpoints). **Rule: small, additive changes only; the default server-start path must never change behavior.**

4. **`internal/auth` — NEW SEAM.** Collaborator touched auth code as part of JWT/security hardening (#53, #64–#66). **Sync before either side next edits internal/auth, internal/auth/manager.go, or internal/auth/middleware.go.**

5. **`internal/dashboard` — NEW SEAM.** Collaborator hardened XSS sinks in dashboard code (#49, #66) as part of ISO 27001 A.14 work. **Sync before Nicolai next edits internal/dashboard/ui.go or token_dashboard.go.**

6. **`internal/distill` — existing seam (since #45).** Still applies: sync before building in it.

7. **`internal/ab` — NEW SEAM (since #78).** Collaborator fixed a TOCTOU race in `StartExperiment` (single `Lock/defer Unlock`, `checkActiveCapLocked` helper, two new concurrent tests). **Sync before either side next edits `internal/ab/engine.go`.**

8. **`internal/povi/challenge*` — NEW SEAM (since #79).** Collaborator rewrote the double-slash guard (atomic INSERT claim-first, `ChallengePending` result, new `UpdateResult` on the `challengeStore` interface + `ChallengeStore`). The `Challenge()` call order changed: `Record` before `Slash`. **Sync before either side next edits `challenge.go`, `challenge_store.go`, or `challenge_test.go`.**

---

## The rules (short version)

1. **GitHub is truth.** Not anyone's Claude's memory, not a notes file. Before building, check what's actually on `origin/main`.
2. **Never push to the other person's branch.** Work crosses between us only as reviewed → merged PRs.
3. **Never edit the other person's migration / recent work.** Find a bug? Hand it back with a precise report.
4. **Session starts with a status sync** (see the standard prompt — run it every time).
5. **Small, frequent, single-purpose PRs.** Short-lived branches. The enemy is a giant branch that has to reconcile against a hundred of the other person's commits.
6. **At a seam, coordinate explicitly** — especially the ledger, auth, and dashboard.

---

## Work ledger — keep current
_(last updated from sync: main at afbe241 — DISTILL 100% COMPLETE)_

### Nicolai + Claude — in progress
- Nothing in active build. DISTILL is fully complete (all of stage 3 shipped). Next roadmap item (token economy Phases 2–5) is GATED on a ledger-seam sync with the collaborator. Highest-leverage non-build move: a first pilot customer.

### Nicolai + Claude — up next (the roadmap — ours, don't take)
- token economy Phases 2–5 (GATED — touches ledger/economy code the collaborator is ACTIVELY in; sync seam #1 before starting).
- SOC2/ISO27001 foundation (codeable groundwork; cert via a vendor like Oneleet only when a customer requires it).
- Replicate the Lens path to the sibling repos (Track / Docs / Code / edge-infra) — after Lens.
- PoVI minting go-live: NOT a build — see preconditions section.
- GO-TO-MARKET (the actual constraint): one pilot customer. Decks + one-pager built; the CTA ("run a pilot, see your number") also produces the real DISTILL savings number.

### Collaborator — recently landed (all merged to main, in our base)
- Auth-reachability fix (#53): AuthMiddleware(ks, m) — DB fast-path unchanged + Manager fallback so the global admin key + JWTs reach admin routes. ES256 JWT (#64/65/66).
- Full ISO27001-track hardening pass (#53→#82, ~29 commits): auth/JWT, TLS, TOCTOU, security headers, CORS (#63), DB atomicity, worker hardening (#82 RLIMIT_AS surfaced by our PR#1 isolator test). Our DISTILL surface survived intact.
- Tenant isolation (#84) — landed mid-our-stage-3, no collision.
- Earlier: stake-atomicity, pessimistic ledger locking (seam #1 satisfied), lock ordering (#32), atomic ExecuteTrade (#34), partitioning (#21/#29), #28 hardening, rate-limiting (#23), control-plane+Redis (#25), PgBouncer, CI/benchmark.
- Stated next initiative: read the other 4 repos (infra first); mock-DB / chaos testing / Lynis on the DB server once Lens is fully done — which is NOW.

### Done (recently merged to main — drops off both lists)
- DISTILL 100% COMPLETE — full feature, live in the request path:
  - Engine + visibility: core (#36) + PDF (#37) + cache/savings (#39) + tiers (#41) + vision-OCR fallback (#44) + dashboard/ROI (#47).
  - Stage 3: worker shipped (#50) + startup proven on linux/amd64 (#51) + preview endpoint (#52) + request-path opt-in (#83) + live VisionDispatcher (#85) + durable token_events attribution (#86, migration 0040).
  - Cardinal rules held end-to-end: inert-by-default; isolated killable worker; no double-counting; vision cost never a saving / never free; no phantom binary token savings.
- Auth-reachability bug (handed to collaborator → fixed in #53).
- Chart audit items + PgBouncer-safe migrations (#30); minor follow-ups (#35); buffered-output-guardrail fix; cleanup.

## Open coordination items
- RESOLVED — STAGE 3 BLOCKER (enforced resource isolation): the collaborator's ProcessIsolator (#45) provided the killable, capped subprocess; DISTILL runs untrusted bytes only through it. pdf.go residual contained. Resolved.
- RESOLVED — DISTILL request-path integration: shipped (#83/#85/#86), additive in internal/proxy, no collision (incl. his #84 mid-arc). His isolator + worker JSON protocol untouched.
- Token economy Phases 2–5 (gated, next big build) — MUST sync seam #1 (extend the PoVI FOR UPDATE pattern, one global lock order) before starting.
- CORS / X-Talyvor-Distill (deferred, his file): per-request distill header not in his #63 allowlist (consistent with Batch/Team; works S2S). Workspace DistillPolicy is the CORS-unaffected primary lever. Adding the header = a 1-line sync on his CORS file, not a unilateral edit.
- costanomaly / dashboard seam — his #28 hardened these; DISTILL panel (#47) additive, didn't touch his render funcs.
- PgBouncer / migrations seam (handled); ledger lock ordering (resolved by him, #32).

---

## PoVI minting — preconditions before go-live
The minting mechanism is BUILT and on main, shipped OFF by default (`LENS_POVI_MINTING_ENABLED=false`; trust-mint retirement switch also not flipped). Flipping it on is an operator/business decision, NOT a build milestone. **Leaving it off costs nothing and carries zero risk — the un-flipped switch is correct, not unfinished.** Preconditions before enabling, all of which are mostly NOT engineering tasks:
1. **A real node network exists** — minting rewards nodes for serving inference; with zero independent operators it mints into a vacuum. Downstream of users/demand (the same constraint as business success).
2. **Security model survived something real** — live concurrent testing (not just pgxmock) + ideally a qualified external security/crypto audit of PoVI. Minting on an unaudited novel economic-security mechanism is the highest-risk action in the codebase.
3. **Token economy complete** — Phases 2–5 built, not just the Phase-1 security model.
4. **A deliberate legal/economic decision on what LENS is** — a mintable value-bearing token has regulatory dimensions (securities/money-transmission, jurisdiction-dependent). Not a code decision.
5. **Enable in a controlled/testnet env first**, then load-bearing.

The trigger is not another build — it's a real network + an audit + a legal call, all downstream of the customer question. (Full note: povi-minting-preconditions.md.)
