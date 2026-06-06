# Talyvor — Work Coordination

**Purpose:** two people build in these repos in parallel, each running Claude / Claude Code. Our Claudes share no memory and cannot see each other's work — **GitHub is the only place our work meets, so GitHub is the single source of truth.** This file is how we avoid double work and handle the seams where our work touches.

**Last synced:** _(update at each session start)_

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

## Product-narrative reframe (v3) — what it changes for the build
The investor/client deck + 8-yr model were reworked to v3. Three shifts that touch the code:
1. **Savings are now stated net of our savings-share fee** on every customer-facing surface — gross inference reduction 56–90% → customer net ~40–65%, with mining pushing toward ~99%. Any dashboard/ROI surface should show gross *and* net, never gross-as-if-the-customer-keeps-it.
2. **Moat = cross-user semantic reuse + cross-provider routing + DISTILL + open-model migration** — explicitly NOT single-provider prefix caching (providers now do that natively, free). Pool-B value comes from *cross-tenant* reuse.
3. **Enterprise data governance is a hard precondition for cross-user pooling** (private-by-default, opt-in, PII-strip, isolation, deletion, TTL, audit). That is exactly what Phase-2 Stage 2.0 builds, sitting on top of the collaborator's tenant isolation (#84).

---

## Work ledger — keep current
_(last updated: Jun 2026 — Phase-2 Stage 2.1 Pool-B royalty mint complete (migration 0043, inert by default); Stage 2.0 exact+semantic gate complete (#89/#90); DISTILL 100% COMPLETE)_

### Nicolai + Claude — in progress
- **Phase-2 Stage 2.1 COMPLETE — Pool-B royalty mint (the ledger seam, seam #1 sync'd with Andrei and approved).** A SERVED cross-tenant pooled hit mints `s × avoided_COGS` to the contributing tenant, **exactly once per serving request**, inert by default (`LENS_POOL_ROYALTY_MINTING_ENABLED=false` default; share `LENS_POOL_ROYALTY_SHARE` default 0.5, validated to [0,1] so Talyvor's net `(1−s)×avoided_COGS ≥ 0` — the Burn-and-Mint invariant; requester-side burn is Stage 2.2). **Idempotency design (the load-bearing part):** new UNPARTITIONED table `pool_royalty_mints` (**migration 0043**, additive/idempotent, povi_challenges shape) with `request_id TEXT NOT NULL UNIQUE` as the key — request_id ALONE, never the matched entry/contributor (a retried request can re-match a different semantic row: ORDER BY similarity LIMIT 1 over a moving 24h window — keying on the match would reintroduce double-mint). Mint flow = ONE transaction: claim `ON CONFLICT (request_id) DO NOTHING` → `RowsAffected()==0` stop → `CreditTx(contributor, s×avoided_COGS, pool_royalty)` → commit. Extends the existing ledger kernel (`CreditTx`→`applyTx` two-step FOR UPDATE) — **no new locking discipline, no advisory lock** (the UNIQUE constraint serializes; repo rule stays pg_advisory_xact_lock-only), plain parameterized SQL (PgBouncer simple-protocol safe). Claim fires AT SERVE, not lookup (an SSE-replay failure that falls through to the live LLM mints zero). Entry identity (exact: pooled cache key; semantic: `prompt_embeddings.id` + similarity, now returned by `GetPooled`) rides on the claim row as attribution DATA. Collisions are deflationary — a reused request_id suppresses, never inflates. **Folded in: the latent PoVI double-mint fix** — `RecordReceipt` now surfaces its ON CONFLICT result and `Processor.Process` refuses to mint a replayed receipt (regression-tested; was inert-but-wrong while minting is off). Guard tests: migration-content audit (claim table unpartitioned + UNIQUE(request_id) present; token_events never gains a request_id UNIQUE; no session advisory lock in any migration).
- **Phase-2 Stage 2.0 COMPLETE (exact + semantic).** The shared-cache governance gate now covers BOTH response caches, opt-in and inert by default. ONE set of switches (no new ones): global `LENS_CACHE_POOLABLE_ENABLED` (default off) + per-workspace `cache_poolable` (default false; exact = migration 0041, semantic = **migration 0042** adding `contributor_workspace_id`/`is_poolable` to `prompt_embeddings`) + admin `PUT /v1/workspaces/{wsID}/cache-poolable`; the read-only `internal/cache_pooling` gate governs both. A pooled hit requires ALL of global+requester+contributor opted-in (contributor verified live → revocation honored). Exact: pooled writes/reads under a NUL-sentinel-disjoint key via `SetWithOwner`/`GetWithOwner`. Semantic: a separate `is_poolable=true` row keyed on the same NUL-sentinel-disjoint `prompt_hash`, the private similarity search filters `is_poolable=false` (so it can never serve a pooled row), and the pooled search filters `is_poolable=true` + reads the contributor. Both: PII never pooled; **zero ledger writes; #84 isolation internals untouched.** Highest-leverage non-build move remains a first pilot customer.

### Nicolai + Claude — up next (the roadmap — ours, don't take)
- **Phase 2 — Pool-B shared-cache royalty economy**, split into safe-now vs gated:
  - **Stage 2.0 governance gate (UNGATED — building now):** see in-progress. Cache-layer only; also the v3 enterprise-objection fix.
  - **Stage 2.1 DONE (seam #1 sync'd + approved):** attribution + the credit-ledger mint path — see in-progress. **Stage 2.2 DONE (fee-shaped, margin-identity):** (a) the margin READ surface — `pool_royalty_margin` view (**migration 0044**, idempotent, derives `margin_usd = avoided_cogs_usd − minted_amount` per claim row) + `poolroyalty.MarginReader` (summary + breakdowns by contributor/requester/layer, dimension allow-listed) — DERIVED from `pool_royalty_mints`, deliberately NOT a token_events write (every customer spend reader sums `cost_usd` with no row-type filter; isolation regression-tested: a served pooled hit writes ZERO token_events rows, with a live-call positive control); (b) `pool_royalty` added to `GetTotalSupply`'s allow-list — royalty LENS honestly in supply, flowing into circulating supply + the LXC fair-rate inputs; `marketplace_fee` (a transfer, not a mint) and `receipt_mine_provisional` (PoVI's own go-live call) stay excluded, pinned by test. No requester debit, no burn, no second balance lock, MintServedHit tx untouched. **The 2.1 supply-accounting flip-on precondition is LIFTED**; remaining flip-on preconditions are business ones (same as PoVI minting). **Stages 2.3–2.5 remain:** PoVI verify / anti-gaming hookup for pool mints (mechanism already built) → USD-pegged redemption.
  - **Pool-B economics — DECIDED (authoritative spec, Nicolai, 2026-06-07):**
    - **Funding source:** the contributor royalty is funded from TALYVOR'S savings-share margin, NEVER from the requester's savings. The customer's net savings (~40-65% by workload) stays fully intact. Pool-B is Talyvor sharing part of its own ~27% cut with the contributor whose cached work created the saving.
    - **Unit:** the contributor is paid in minted LENS.
    - **Shape:** FEE-SHAPED, not burn. The word "burn" is retired for Pool-B. There is NO requester-side LENS debit and NO token destruction. The requester already pays via the existing USD savings-share fee, which lives outside the LENS ledger.
    - **`(1−s)` treatment:** MARGIN-IDENTITY ONLY. Mint `s × avoided_COGS` LENS to the contributor. Talyvor's `(1−s) × avoided_COGS` is its USD savings-share margin — it is NOT minted to a Talyvor LENS wallet. LENS is purely the contributor-reward token. *(Correction, Stage 2.2: there was no pre-existing USD cost-accounting in the repo to track this — the savings-share fee is a commercial concept outside the code. Stage 2.2 CREATES the first on-repo margin read-surface, derived from `pool_royalty_mints` as `avoided_cogs_usd − minted_amount` — the `pool_royalty_margin` view + `MarginReader` — kept fully separate from every customer spend surface.)*
    - **Locking consequence:** Stage 2.2 performs ONE balance write (the contributor credit) plus the claim row. No requester balance operation, no two-row lock, no #32 two-row-ordering question for Pool-B.
- SOC2/ISO27001 foundation (codeable groundwork; cert via a vendor like Oneleet only when a customer requires it).
- Replicate the Lens path to the sibling repos (Track / Docs / Code / edge-infra) — after Lens.
- PoVI minting go-live: NOT a build — see preconditions section.
- GO-TO-MARKET (the actual constraint): one pilot customer. Decks + one-pager built; the CTA ("run a pilot, see your number") also produces the real DISTILL savings number.
- **Future (Phase-3-adjacent) — token-efficiency & cache-hit-rate optimization (Talyvor-side, model-agnostic).** Goal: rewrite a user's job into the minimal form that uses the fewest tokens, hits the cross-user cache precisely, and loses no quality. Real, buildable levers — all operate in NORMAL language (what tokenizes cheaply and what the model + cache understand):
  (a) SEMANTIC NORMALIZATION before cache lookup — deterministically canonicalize equivalent requests (casing, filler, phrasing; optional embed-and-cluster of near-duplicates) so 'capital of France?' and 'france capital' collapse to one cache key and the later request HITS instead of recomputing. Every extra hit is a full model run avoided — the highest-leverage item, compounds the cross-user cache.
  (b) PROMPT COMPRESSION on the miss path (LLMLingua-style token trimming) — bounded real savings, quality-gated by threshold. Same family as DISTILL.
  REJECTED on first principles (recorded so they aren't re-proposed):
  - A model-weight 'zipper' / x100000 lossless compressor — forbidden by the pigeonhole/Shannon counting limit. Big shrink is only possible LOSSY (quantization, distillation, pruning).
  - A 2-symbol alphabet (binary/Morse) as an intermediate form to cut tokens — INVERTS the goal: few symbol-TYPES means many tokens, because the tokenizer shatters a long 2-symbol stream into many tiny tokens. Normal-language tokens are cheap precisely BECAUSE the vocabulary is large (each token packs more meaning). Round-tripping through binary adds two lossy conversions and the model never sees it anyway (it's converted back to normal language first), so it buys nothing.
  THE ONE LEGITIMATE 'Talyvor alphabet' — a CUSTOM TOKENIZER trained on Talyvor's own traffic, inside a Phase-6 Talyvor-trained model: make the multi-word patterns our customers repeat into single tokens. This genuinely cuts tokens — but the win comes from MORE vocabulary (not fewer symbols) and from TRAINING the model to use it (not secrecy), so it lives in the Phase-6 specialized-model project where alphabet and model are co-designed. Logged so the full idea is captured and correctly resolved.

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
- **Phase-2 Stage 2.0 COMPLETE — shared-cache governance gate (exact + semantic).** Cross-tenant pooling for BOTH response caches: opt-in (global `LENS_CACHE_POOLABLE_ENABLED` + per-workspace `cache_poolable`), read-only `internal/cache_pooling` gate, NUL-sentinel-disjoint pooled keyspaces, contributor provenance + live consent, all inert by default and additive on top of #84 (reads the resolved wsID). Migrations **0041** (workspaces.cache_poolable) + **0042** (prompt_embeddings.contributor_workspace_id/is_poolable) follow seam #2 (additive, own files). **No ledger writes → does NOT touch seam #1.**
- **🔔 HEADS-UP TO COLLABORATOR (Andrei):** the cache layer is in motion — migrations **0041** (workspaces) + **0042** (`prompt_embeddings`) add the pooling columns, and an opt-in cross-tenant pooling path landed for both the exact and semantic caches (additive, default-off, #84 isolation untouched). Pool-B royalty **minting stays gated on seam #1** (no ledger code in this work). Sync before touching the cache layer or `prompt_embeddings`.
- **Phase-2 Stage 2.1 — Pool-B credit-ledger mint: DONE (seam #1 sync'd with Andrei, approved, extended — not reinvented).** The mint extends his `CreditTx`→`applyTx` FOR UPDATE kernel with zero new locking (UNIQUE(request_id) claim on the new unpartitioned `pool_royalty_mints`, migration 0043; one tx per mint; no advisory locks; PgBouncer-safe). His latent PoVI replay double-mint (RecordReceipt conflict result ignored) fixed in the same PR with the same guard shape. **Stage 2.2 DONE** (fee-shaped/margin-identity per "Pool-B economics — DECIDED"): margin read-surface (view 0044 + MarginReader, derived — zero token_events writes, isolation regression-tested) + `pool_royalty` counted in `GetTotalSupply`. Ledger touch was read-list-only; the mint tx is byte-identical to 2.1. **Stages 2.3–2.5 stay gated on seam #1.**
- **🔔 2026-06-07 — Heads-up to Andrew (seam #1):** the Stage 2.2 requester-side burn is CANCELLED as previously framed. Pool-B is fee-shaped (margin-identity) — contributor gets minted LENS, Talyvor's `(1−s)` stays USD margin. Net effect: Stage 2.2 does NOT lock two balance rows; the two-row #32 ordering you were warned about for Pool-B is no longer coming. Only single-row contributor credits touch the ledger.
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
