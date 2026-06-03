# Talyvor — Work Coordination

**Purpose:** two people build in these repos in parallel, each running Claude / Claude Code. Our Claudes share no memory and cannot see each other's work — **GitHub is the only place our work meets, so GitHub is the single source of truth.** This file is how we avoid double work and handle the seams where our work touches.

**Last synced:** 2026-06-03 — main at 36d8ee3 (collaborator security hardening batch complete)

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

_(last updated: 2026-06-03, main at 36d8ee3)_

### Nicolai + Claude — in progress
- DISTILL stage 3 request-path integration: PRs #50, #51, #52 are on main (distill-worker image, smoke test, admin preview endpoint). Current status unknown — sync with Nicolai before touching internal/distill or internal/proxy.

### Nicolai + Claude — up next (the roadmap — his, don't take)
- ⚠️ **[touched for security by collaborator — see note]** JWT / auth: ES256 JWT (A3) is on main (#64/#65/#66). Collaborator replaced HS256 with EC P-256 asymmetric signing, added `/v1/auth/jwks`, hard-errors on `LENS_JWT_SECRET`. Nicolai does **not** need to rebuild this. If auth behaviour needs changing, sync first.
- ⚠️ **[touched for security by collaborator — see note]** XSS gaps: dashboard XSS hardened (#49, #66) — `escapeHTML` on all client-controlled strings, `encodeURIComponent` on URL segments, `escapeHTML` added to `commonHead`. Nicolai should verify whether any of his own new code paths have outstanding XSS gaps not covered by this pass.
- token economy Phases 2–5 (⚠️ GATED — touches ledger/economy code the collaborator is ACTIVELY in; sync seam #1 before starting)
- SOC2 foundation (codeable groundwork; cert itself = vendor, only when customer requires)
- PoVI minting go-live: NOT a build — see preconditions section
- Track: sprint activation · @mentions · budget UI
- Docs: identity/recipient sync · @mentions
- Code: finish JetBrains plugin
- Suite: replicate Helm to Track/Docs/Code

### Collaborator (Andrei) — recently landed (since last sync at 0ae7872)

**Transport security (ISO 27001 A.13):**
- HTTPS / Let's Encrypt (#56): TLS 1.2/1.3, HSTS, HTTP→HTTPS redirect on `cmd/lens`
- Postgres TLS (#57, #58): `LENS_DB_SSL_MODE` defaults to `require`; sslmode validated in migrate path; port-80 failure escalated on first boot
- Redis TLS (#62): `LENS_REDIS_TLS` / `rediss://` enforcement

**Application security (ISO 27001 A.9, A.14):**
- HTTP security headers (#63): CSP, X-Frame-Options, X-Content-Type-Options, Referrer-Policy, Permissions-Policy; CORS `Access-Control-Expose-Headers`
- [touched for security] JWT ES256 (#64/#65/#66): HS256 → EC P-256 asymmetric signing; `LENS_JWT_PRIVATE_KEY`; `/v1/auth/jwks` endpoint; `LENS_JWT_SECRET` hard-errors at startup
- [touched for security] Auth middleware fix (#53): `AuthMiddleware` taught to accept global key and JWTs via Manager fallback (required for ES256 wiring)
- [touched for security] XSS hardening (#49, #66): `escapeHTML` on all client-controlled strings in dashboard `apply*` render functions; `encodeURIComponent` on URL segments; `escapeHTML` definition added to `commonHead` (tokens/nodes pages were missing it)
- bcrypt on cache node secrets (#66): `node_secret` hashed before DB storage (was plaintext)
- xDS workspace isolation (#67): all 6 node mutation endpoints (3× heartbeat + 3× DELETE) scoped to `AND workspace_id = $N` — prevents cross-workspace node hijacking

**Resilience / HA / operations (ISO 27001 A.12):**
- HA bugs (#61): zombie process reaping, `shared_breaker` data race, HA clock-skew
- Control-plane snapshot fix (#60): rows closed before next `pool.Query` — prevents self-deadlock under pool pressure
- xDS HA — HeartbeatStore + Redis liveness (#42 + earlier): heartbeat reuse across instances on failover; `isLive()` dual-signal (Redis primary, Postgres fallback)
- Distill worker hardening (#67): `io.LimitReader(stdout, 32 MiB)` caps parent heap; stderr captured into `bytes.Buffer` + re-emitted via `slog.Warn` (log injection prevention)
- Snapshot scan error logging (#67): malformed DB rows now `slog.Warn` instead of silent `continue`
- Code-review fixes (#54): 7 issues across process isolation, control plane, DB

**DB / concurrency / migrations:**
- DB transaction atomicity (#66): `Stake()`, `Unstake()` (`DELETE…RETURNING` eliminates TOCTOU double-credit), `RegisterNode()`, cache-node registration + heartbeat — all now single `pgx.Tx`
- Migration 0034 fix (#66): removed stray `BEGIN`/`COMMIT` that broke migration runner transaction boundary (collaborator's own migration)
- Migration 0038 (#66): new migration — FK index on `marketplace_trades(listing_id)`; `session_turns` FK → `ON DELETE CASCADE`
- Migration no-transaction marker (#59): `lens:no-transaction` for 0037 `DROP INDEX CONCURRENTLY` — fixes crash on fresh apply

### Collaborator (Andrei) — up next (security hardening remaining)
- **Dashboard auth gate (A4)** — admin/monitoring dashboard has no authentication. Direct ISO 27001 A.9 gap.
- **NATS TLS** — inter-service messages travel in plaintext. ISO 27001 A.13.
- **Mining node TLS** — `cmd/node`, `cmd/cachenode`, `cmd/embednode` serve plain HTTP only. Same gap as `cmd/lens` had before #56.
- **API key rotation TOCTOU** — `/v1/workspaces/{wsID}/api-keys/{keyID}/rotate` is non-atomic; concurrent calls can produce two simultaneously active keys. ISO 27001 A.9.
- **Annotation submission TOCTOU** — `SubmitAnnotation()` lacks `SELECT FOR UPDATE` around task lookup + stake check; concurrent submissions can double-insert. Same class as Stake/Unstake race already fixed.
- **Embedding node `node_secret_hash` always NULL** — cache and inference nodes got bcrypt secrets; embedding nodes do not. Inconsistent auth posture.
- **`X-Request-ID` log injection** — untrusted client header reflected verbatim in structured logs. Low severity. ISO 27001 A.12.4.

### Done (recently merged — for reference)
- DISTILL engine + visibility complete: core (#36) + PDF (#37) + cache/savings (#39) + tiers (#41) + vision-OCR fallback (#44) + dashboard/ROI (#47)
- Stake-listing atomicity (a9cd852)
- Pessimistic locking on ledger writes (extended existing PoVI `FOR UPDATE` pattern)
- Global lexicographic lock ordering in `Transfer()` (#32)
- `atomic ExecuteTrade` (#34), hash partitioning (#21, 0034), rate-limiting (#23), control-plane + Redis routing (#25, 0035), multi-process readiness, PgBouncer, CI/benchmark

---

## Open coordination items

- **`internal/auth` is now a SHARED SEAM** — collaborator's security work (#53, #64–#66) is the first substantive change to auth. Nicolai: sync before next touching `internal/auth`, `Manager`, or `Middleware`. Collaborator: same.
- **`internal/dashboard` is now a SHARED SEAM** — collaborator's XSS hardening (#49, #66) edited `ui.go` and `token_dashboard.go`. Nicolai: sync before next editing dashboard files.
- **`internal/distill` shared seam (since #45)** — still applies. Collaborator's process-isolation hardening (#67) also touched `isolator.go`. Sync before either side edits distill.
- **DISTILL stage 3** — #50/#51/#52 on main. Current Nicolai progress unknown; collaborator should not touch `internal/proxy` or `internal/distill` request-path wiring without syncing first.
- **Token economy Phases 2–5 (gated)** — collaborator's DB atomicity work (#66) touched `Stake()`/`Unstake()` (token-economy code). Seam #1 applies. Sync before Nicolai starts Phases 2–5.
- **Ledger lock ordering (resolved, #32)** — global lexicographic ordering in place. No longer open.
- **PgBouncer / migrations seam (handled)** — migrate Job takes explicit `migrations.databaseURL`; pooler untouched.

---

## PoVI minting — preconditions before go-live
The minting mechanism is BUILT and on main, shipped OFF by default (`LENS_POVI_MINTING_ENABLED=false`; trust-mint retirement switch also not flipped). Flipping it on is an operator/business decision, NOT a build milestone. **Leaving it off costs nothing and carries zero risk — the un-flipped switch is correct, not unfinished.** Preconditions before enabling, all of which are mostly NOT engineering tasks:
1. **A real node network exists** — minting rewards nodes for serving inference; with zero independent operators it mints into a vacuum. Downstream of users/demand (the same constraint as business success).
2. **Security model survived something real** — live concurrent testing (not just pgxmock) + ideally a qualified external security/crypto audit of PoVI. Minting on an unaudited novel economic-security mechanism is the highest-risk action in the codebase.
3. **Token economy complete** — Phases 2–5 built, not just the Phase-1 security model.
4. **A deliberate legal/economic decision on what LENS is** — a mintable value-bearing token has regulatory dimensions (securities/money-transmission, jurisdiction-dependent). Not a code decision.
5. **Enable in a controlled/testnet env first**, then load-bearing.

The trigger is not another build — it's a real network + an audit + a legal call, all downstream of the customer question. (Full note: povi-minting-preconditions.md.)
