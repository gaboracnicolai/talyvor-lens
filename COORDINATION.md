# Talyvor — Work Coordination

**Purpose:** two people build in these repos in parallel, each running Claude / Claude Code. Our Claudes share no memory and cannot see each other's work — **GitHub is the only place our work meets, so GitHub is the single source of truth.** This file is how we avoid double work and handle the seams where our work touches.

**Last synced:** 2026-06-04 — main at f4306ae (#75 — mining node TLS, last of the ISO 27001 hardening batch)

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

_(last updated: 2026-06-04, main at f4306ae / #75)_

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

### Collaborator (Andrei) — recently landed (since last sync at 36d8ee3 / #68)

**Transport security (ISO 27001 A.13):**
- HTTPS / Let's Encrypt (#56): TLS 1.2/1.3, HSTS, HTTP→HTTPS redirect on `cmd/lens`
- Postgres TLS (#57, #58): `LENS_DB_SSL_MODE` defaults to `require`; sslmode validated in migrate path; port-80 failure escalated on first boot
- Redis TLS (#62): `LENS_REDIS_TLS` / `rediss://` enforcement
- NATS TLS (#69): inter-service messages now encrypted; `NATS_TLS_CERT`/`NATS_TLS_KEY`/`NATS_TLS_CA` env vars; connection refuses on TLS failure
- Mining node TLS (#75): all three node binaries (`cmd/node`, `cmd/cachenode`, `cmd/embednode`) now support opt-in TLS via env vars (`NODE_TLS_CERT`/`NODE_TLS_KEY` etc.); plain HTTP path logs a startup ⚠️ warning; 12 new tests across the three binaries

**Application security (ISO 27001 A.9, A.14):**
- HTTP security headers (#63): CSP, X-Frame-Options, X-Content-Type-Options, Referrer-Policy, Permissions-Policy; CORS `Access-Control-Expose-Headers`
- [touched for security] JWT ES256 (#64/#65/#66): HS256 → EC P-256 asymmetric signing; `LENS_JWT_PRIVATE_KEY`; `/v1/auth/jwks` endpoint; `LENS_JWT_SECRET` hard-errors at startup
- [touched for security] Auth middleware fix (#53): `AuthMiddleware` taught to accept global key and JWTs via Manager fallback (required for ES256 wiring)
- [touched for security] XSS hardening (#49, #66): `escapeHTML` on all client-controlled strings in dashboard `apply*` render functions; `encodeURIComponent` on URL segments; `escapeHTML` definition added to `commonHead` (tokens/nodes pages were missing it)
- bcrypt on cache node secrets (#66): `node_secret` hashed before DB storage (was plaintext)
- bcrypt on embedding node secrets (#72): embedding nodes were missed in the first bcrypt pass; `node_secret_hash` now populated consistently across all three node types
- xDS workspace isolation (#67): all 6 node mutation endpoints (3× heartbeat + 3× DELETE) scoped to `AND workspace_id = $N` — prevents cross-workspace node hijacking
- Dashboard / admin gate (#73): `/metrics` and `/ha/status` endpoints now require `X-Admin-Key` header; previously open to any caller on the network (ISO 27001 A.9)
- `X-Request-ID` sanitisation (#74): untrusted header sanitised to alphanumeric + hyphens + underscores, capped at 64 bytes — prevents log injection and oversized ID storage (ISO 27001 A.12.4)

**DB / atomicity / TOCTOU:**
- DB transaction atomicity (#66): `Stake()`, `Unstake()` (`DELETE…RETURNING` eliminates TOCTOU double-credit), `RegisterNode()`, cache-node registration + heartbeat — all now single `pgx.Tx`
- API key rotation TOCTOU (#71): `/v1/workspaces/{wsID}/api-keys/{keyID}/rotate` rewritten as single atomic transaction — concurrent calls no longer produce two simultaneously active keys (ISO 27001 A.9)
- Annotation submission TOCTOU (#70): `SubmitAnnotation()` now wraps task lookup + stake check + insert in `SELECT FOR UPDATE` — concurrent submissions can no longer double-insert (ISO 27001 A.9)
- Migration 0034 fix (#66): removed stray `BEGIN`/`COMMIT` that broke migration runner transaction boundary (collaborator's own migration)
- Migration 0038 (#66): new migration — FK index on `marketplace_trades(listing_id)`; `session_turns` FK → `ON DELETE CASCADE`
- Migration no-transaction marker (#59): `lens:no-transaction` for 0037 `DROP INDEX CONCURRENTLY` — fixes crash on fresh apply

**Resilience / HA / operations (ISO 27001 A.12):**
- HA bugs (#61): zombie process reaping, `shared_breaker` data race, HA clock-skew
- Control-plane snapshot fix (#60): rows closed before next `pool.Query` — prevents self-deadlock under pool pressure
- xDS HA — HeartbeatStore + Redis liveness (#42 + earlier): heartbeat reuse across instances on failover; `isLive()` dual-signal (Redis primary, Postgres fallback)
- Distill worker hardening (#67): `io.LimitReader(stdout, 32 MiB)` caps parent heap; stderr captured into `bytes.Buffer` + re-emitted via `slog.Warn` (log injection prevention)
- Snapshot scan error logging (#67): malformed DB rows now `slog.Warn` instead of silent `continue`
- Code-review fixes (#54): 7 issues across process isolation, control plane, DB

### Collaborator (Andrei) — open PRs (pending merge, as of 2026-06-04)

These were created after a full codebase audit (2026-06-04) and are open PRs on `origin`. Not yet on main.

- **PR #78 — AB engine TOCTOU** (`internal/ab/engine.go`, `engine_test.go`): `StartExperiment` used a double-lock pattern (read lock to check status, release, re-acquire write lock) that allowed two goroutines to race past both the "completed" check and the active-cap check. Fixed by collapsing to a single `Lock/defer Unlock` with a `checkActiveCapLocked` helper. Two new concurrent tests prove the fix.
- **PR #79 — PoVI double-slash TOCTOU** (`internal/povi/challenge.go`, `challenge_store.go`, `challenge_test.go`): In HA deployments two Lens instances could both pass the `AlreadyChallenged` SELECT and each call `Slash` for the same receipt. Fixed by making `Record` the atomic claim (INSERT; `ON CONFLICT DO NOTHING`; `RowsAffected==0` → `ErrAlreadyChallenged`). `Challenge()` now claims first, then fetches paths, then slashes, then calls new `UpdateResult`. `ChallengePending` added as a transient result state. New test `TestChallenge_NoConcurrentDoubleSlash` (20 goroutines, 1 slash expected).
- **PR #80 — X-Node-Secret timing oracle** (`cmd/node/server.go`, `cmd/cachenode/server.go`, `cmd/embednode/server.go`): Secret checked with Go's `!=` string operator, which short-circuits on first byte mismatch — allows timing-based byte-by-byte recovery. Replaced with `crypto/subtle.ConstantTimeCompare` on all three node handlers.

### Collaborator (Andrei) — up next
All previously listed items are now done (#69–#75). No known remaining hardening items. Next priorities follow from whatever Nicolai identifies or from a future audit pass.

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
- **`internal/ab` is now a SHARED SEAM (since #78, open PR)** — collaborator fixed a TOCTOU race in `StartExperiment`. If Nicolai's roadmap includes any AB engine changes, sync before merging #78 or making further edits to `engine.go`.
- **`internal/povi/challenge*` is now a SHARED SEAM (since #79, open PR)** — collaborator changed the `challengeStore` interface (added `UpdateResult`) and reordered `Challenge()`. Any PoVI Phase work that touches the challenge flow needs to sync against #79 before starting.
- **Open PRs #78 / #79 / #80** — all three are ready to review/merge. No conflicts with main known at time of writing (branched from f4306ae).
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
