# BUILD_STATE.md — Talyvor Lens canonical build-state manifest

**Single source of truth for "what is built," derived from the actual code at the SHA below — never from the roadmap, notes, or assumptions.** Regenerated (never hand-edited) whenever it goes stale.

- **Derived from main:** `6d36725`
- **Latest migration:** `0066_reputation_events.sql`
- **Config:** all `config.go:LINE` citations are `internal/config/config.go`.
- **Method:** every cell was grep'd / read from code. Where code and a note/roadmap disagree, **the code wins** — see [§C Discrepancies](#c-discrepancies-code-wins).

## Status legend
- **BUILT-&-ON** — built and active by default (no flag, or its flag/default is on).
- **BUILT-INERT** — fully built but behind a **default-off** flag (or no rows exist until one flips). The default posture.
- **PARTIAL** — built but cannot operate as-is (needs substrate that doesn't exist single-box).
- **ABSENT** — no code (or a deliberately omitted analog).

---

## The master kill-switch (read this first)

`LENS_ECONOMY_ENABLED` — **default TRUE** (`config.go:1130` sets `c.EconomyEnabled = true`, overridden only if the env var is explicitly set, `:1131-1133`). When **false**, the block at `config.go:1134` force-sets **10** flags false regardless of their own env:

`PatternMiningEnabled · PatternCaptureEnabled · PatternEarningEnabled · PoolRoyaltyMintingEnabled · POVIMintingEnabled · TrustfulComputeMintEnabled · CacheSharingEnabled · CachePoolableEnabled · DistillPoolableEnabled · RoutingIntelligenceEnabled`

**Deliberately NOT force-offed** (documented exceptions): `LXCGatingEnabled` / `LXCShadowSpendEnabled` (fiat-pegged, not the token economy), the **U6 floor + rate cap** (safety restrictions), `WorkTierEnabled` / `GuardrailsEnabled` (non-economic). The manifest test `cmd/lens/economy_killswitch_test.go` (unit/config — no PG) asserts the force-off set and that LXC stays wired.

---

## §A — Component build-state

### A1 · PoVI + node-earning
| Component | Status | Gating flag + default | Key files (file:line) | Migration | real-PG test | Last SHA |
|---|---|---|---|---|---|---|
| PoVI mint kernel | BUILT-INERT | `LENS_POVI_MINTING_ENABLED` **false** (config.go:622) | `internal/povi/mint.go:50` `MintFromReceipt` (gate `:51`) | 0019 ledger | indirect | (povi pkg) |
| Receipts (ed25519) + Merkle | BUILT-&-ON | none (crypto primitive) | `receipt.go:109/118`, `merkle.go:78/96/128` | — | unit | — |
| Stake + **real Slash** | BUILT-&-ON | `LENS_POVI_MIN_STAKE` **100.0** (config.go:779) | `internal/povi/stakes.go:303`; `internal/mining/stake_ledger.go:94` | 0032 `povi_stakes` | **yes** (`stakes_concurrency_integration_test.go`) | — |
| Challenge-and-slash + scheduler | BUILT-INERT | `LENS_POVI_CHALLENGE_RATE` (no-op at 0); needs a live node | `challenge.go:216`, `challenge_scheduler.go` | 0033 `povi_challenges` | indirect | — |
| Receipts processor + HTTP route | BUILT-INERT | reads `cfg.POVIMintingEnabled` | `internal/povi/processor.go:65`; route `cmd/lens/main.go:3072` → `.Process` | 0031 `povi_receipts` | indirect | c859226 |
| **node-earning** (`cmd/node` daemon) | **PARTIAL (needs substrate)** | the PoVI flag + a **running node** | `cmd/node/main.go:31` (daemon, serves `/challenge`) | — | n/a | — |
| Legacy trust-mint (`RecordServedRequest`) | **ABSENT (dead path)** | `LENS_TRUSTFUL_COMPUTE_MINT_ENABLED` **false** (config.go:964) | `internal/mining/compute_mining.go:357` via `Router.NotifyServed` (`internal/localrouter/multi.go:136`) | 0020 | — | — |

> **node-earning** code is complete + wired (daemon registers, submits receipts → `poviProcessor.Process` → `MintFromReceipt`), but the mint requires the verifier↔prover round-trip — a **running `cmd/node`** to answer challenges. Inert-complete in code, **needs substrate to operate**. **`NotifyServed` has ZERO callers** (def-only at `multi.go:136`; `main.go` comment confirms "no caller today") → the legacy trust-mint path is dead in production.

### A2 · Cache (Pool-B) royalty — `internal/poolroyalty/` (11 components)
Mint components gate on `LENS_POOL_ROYALTY_MINTING_ENABLED` **false** (config.go:624); `MintServedHit` re-checks per call. All `*_integration_test.go` gate on `LENS_TEST_DATABASE_URL`.
| # | Component | Status | file:line | Migration | real-PG | Last SHA |
|---|---|---|---|---|---|---|
| 1 | Minter `MintServedHit` | BUILT-INERT | `minter.go:255` | 0043 `pool_royalty_mints` | yes (`cap_`, `linkage_`) | — |
| 2 | Per-pair cap `capCountSQL` | BUILT-INERT | `minter.go:135` (used :348) | 0043 | yes (`cap_integration_test.go`) | — |
| 3 | Per-entry cap `entryCountSQL` | BUILT-INERT | `minter.go:149` (used :370) | 0047 entry index | yes | — |
| 4 | Volume detector | BUILT-&-ON (read-only) | `detector.go:163` | 0043 | yes (`detector_integration_test.go`) | — |
| 5 | Bilateral detector | BUILT-&-ON (read-only) | `detector.go:206` | 0043 | yes | — |
| 6 | Similarity detector | BUILT-&-ON (read-only) | `detector.go:245` | 0042 embeddings | yes | — |
| 7 | Margin view | BUILT-&-ON (read-only; **wired** §A11) | `margin.go:75` → view `pool_royalty_margin` | 0044 (status appended 0046) | yes (handler #227) | 1024f15 |
| 8 | Adjudication | BUILT-INERT | `adjudication.go` `Adjudicate` | 0048 `pool_royalty_adjudications` | yes (`adjudication_integration_test.go`) | c859226 |
| 9 | Revoker (CAS held→revoked) | BUILT-INERT | `revoker.go:121` | 0046 holdback/status | yes (`revoker_integration_test.go`) | c859226 |
| 10 | Resolver (flag→candidates) | BUILT-&-ON (read-only) | `resolver.go:146/158/172` | 0043 | yes (`resolver_integration_test.go`) | — |
| 11 | Finalize sweeper | **BUILT-&-ON (NOT gated)** | `sweeper.go:117` (`StartScheduler:206`) — ungated so held LENS can't strand | 0046 | yes (indirect) | 302dc48 |

> The cache **detect / resolve / margin** readers (rows 4–7, 10) are now **wired** — on-demand admin endpoints (#227 `1024f15`) + the automatic leader-elected sweep (#230 `11aabe8`). See **§A11**.

### A3 · Distill reuse-royalty — `internal/poolroyalty/distill_*` + `internal/proxy/distill_integration.go`
| Component | Status | Gating flag + default | file:line | Migration | real-PG | Last SHA |
|---|---|---|---|---|---|---|
| PR1 cross-tenant OCR pool | BUILT-INERT | `LENS_DISTILL_POOLABLE_ENABLED` **false** (config.go:620) + per-WS dual consent | `internal/proxy/distill_integration.go:270-307` | (uses 0052 attrib) | no (httptest) | 025c3fc |
| PR2 avoided-COGS basis | BUILT-&-ON (descriptive, **no mint**) | none (writes once a consented serve occurs) | `internal/distillattrib/store.go:87` | 0061 `distill_royalty_basis` | yes (`basis_test.go`) | 025c3fc |
| PR3 gated mint `DistillMinter` | BUILT-INERT | `LENS_POOL_ROYALTY_MINTING_ENABLED` **false** (config.go:624) | `distill_minter.go` (wired `main.go:677`) | 0062 `distill_royalty_mints` (request_id UNIQUE) | yes (`distill_minter_integration_test.go`) | 8b26091 |
| **caps** (per-pair / per-content) | BUILT-&-ON (deflationary; **0/0 = off**) | `LENS_DISTILL_MINT_CAP_PER_PAIR`/`_PER_CONTENT` **0** (config.go:828), window **24h** (config.go:844) | `distill_minter.go` `SetCap`/`SetContentCap` (wired `main.go:684-685`) | (over 0062) | yes (`distill_caps_integration_test.go`) | 8b26091 |
| **detector** (volume+bilateral, observe-only) | BUILT-&-ON (read-only; **wired** §A11) | none (read-only; thresholds `Detect*`) | `distill_detector.go:83` — **NO similarity** (deliberate, header :24-29); `GET /v1/admin/distill-royalty/detect` (#228) + the sweep (#230) | (reads 0062) | yes (`distill_detector_integration_test.go`) | b3e40e1 |
| **resolver** (volume→`content_swarm`, self_dealing→`pair_coarse`) | BUILT-&-ON (read-only; held-only) | none | `distill_resolver.go:68` `NewDistillResolver` (no similarity); `GET /v1/admin/distill-royalty/resolve` `main.go:1354` | (reads 0062) | yes (`distill_resolver_integration_test.go`) | 5bbc81d (#229) |
| **revoke / adjudication** | BUILT-&-ON (endpoint live; doubly-inert) | admin auth + held rows (need the mint flag) | `revoker.go` `NewRevokerForTable`; `adjudication.go` `NewAdjudicationWriterForTable`; route `POST /v1/admin/distill-royalty/adjudicate` `main.go:1310` | 0063 `distill_royalty_adjudications` | yes (`distill_revoke_integration_test.go`) | c859226 |
| **margin view** | BUILT-&-ON (read-only; **wired** §A11) | none | `distill_margin.go:21` `DistillMarginReader`; `GET /v1/admin/distill-royalty/margin` `main.go:1355` | 0064 `distill_royalty_margin` view | yes (`distill_margin_integration_test.go`) | 2048919 |

> Distill now has **full parity** with the cache royalty's anti-gaming + observability set — detector, **resolver**, margin, revoke/adjudication, all **wired** (admin endpoints #228/#229 + the automatic sweep #230; see §A11). Two honest data-model deviations remain: **no similarity detector/resolver** (distill OCR is exact-`content_hash`; no similarity distribution) and the volume detector/resolver are reframed as the **sock-puppet-swarm** signal (once-per-relationship ⇒ `mints == distinct requesters`).

### A4 · Pattern economy — `internal/mining/pattern_mining.go` + `internal/proxy/pattern_*`
| Component | Status | Gating flag + default | file:line | Migration | real-PG | Last SHA |
|---|---|---|---|---|---|---|
| S1 rarity bound | BUILT-&-ON (always applied in miner) | none | `pattern_mining.go:307` `ScoreRarity` | — | yes (`pattern_rarity_bound_test.go`) | — |
| S2 per-window earn cap | BUILT-&-ON | `LENS_PATTERN_EARN_CAP_PER_WORKSPACE` **50000** (a real limit), window **24h** | `pattern_mining.go:370` (`SetEarnCap:239`) | — | yes (`pattern_earn_cap_test.go`) | — |
| S3 idempotency claim | BUILT-&-ON (structural) | none | claim-first write | 0049 `pattern_mine_credits` (UNIQUE `(request_id, workspace_id)`) | yes (`pattern_earn_idempotency_test.go`) | — |
| S4 earn wire-up | BUILT-INERT | `LENS_PATTERN_EARNING_ENABLED` **false** (config.go:628) | `internal/proxy/pattern_earn.go:58` → `proxy.go:1331` | (uses 0049) | no (in-mem; covered in `mining`) | — |
| Capture path | BUILT-INERT | `LENS_PATTERN_CAPTURE_ENABLED` **false** (config.go:627) | `internal/proxy/pattern_capture.go:74` | — | no (in-mem) | — |
| Mining opt-in route | BUILT-INERT | `LENS_PATTERN_MINING_ENABLED` **false** (config.go:621) | route `main.go:2331` | — | (mining pkg) | — |

### A5 · LXC billing (fiat — independent of the token economy)
| Cell | Value |
|---|---|
| Status | **BUILT-INERT** — full Stripe checkout/webhook/refund/idempotency, default off |
| Flag | `LENS_BILLING_ENABLED` **false** (config.go:637); **requires BOTH** `LENS_STRIPE_SECRET_KEY` + `LENS_STRIPE_WEBHOOK_SECRET` or startup fails (config.go:706-710) |
| Code | `internal/billing/billing.go` (+ `stripe_live.go`); admin path is **read-only list** (`GET /v1/admin/billing/purchases`, `main.go:1869`) — no manual-credit endpoint |
| Table | `lxc_purchases` — migration **0054** (`INSERT … ON CONFLICT (stripe_event_id) DO NOTHING`) |
| LXC gating / shadow | `LENS_LXC_GATING_ENABLED` **false** (config.go:626) · `LENS_LXC_SHADOW_SPEND_ENABLED` **false** (config.go:625) — **NOT** economy-killswitched (config.go:1144-1153) |
| real-PG test | **yes** (`internal/billing/billing_integration_test.go`, runs migration 0054, money idempotency + concurrency) |

### A6 · Routing-intelligence
**BUILT-INERT** · `LENS_ROUTING_INTELLIGENCE_ENABLED` **false** (config.go:635), **in the kill-switch block**. Feeds pattern aggregates into auto-route selection; live only on `auto`/`X-Talyvor-Auto-Route` requests (`internal/routing/routing.go`, `proxy.go:992`). Pinned models unaffected. The per-request `Recommend` is an in-memory map lookup (never a DB query); the corpus loads on a timer via `mining.PatternMiner.AggregateCohorts` (`pattern_mining.go:679`, `aggregateCohortsSQL`). real-PG test: **yes** — `internal/mining/aggregate_cohorts_integration_test.go` (#236 `6d36725`) executes `aggregateCohortsSQL` against real `routing_patterns`, proving the opt-out **privacy exclusion** (`opted_in=FALSE` rows excluded from COUNT/AVG/COUNT-DISTINCT; an all-opted-out cohort never surfaces) + aggregation/grouping; the Advisor's in-memory ranking + floors stay unit-tested (`routing_test.go`, `mockSource`).

### A7 · WorkTier
**BUILT-INERT** (capture) · `LENS_WORKTIER_ENABLED` **false** (config.go:633), **NOT** in the kill-switch block (descriptive ⇒ off=safe). **Doctrine enforced in code:** mint-free by construction (`internal/worktier/worktier.go:3-8`, import-guard test fails if mining/economy/ledger imported; `worktier.Store` is Exec/Query-only, no `Begin`/ledger). Write-only post-flush to `work_tier_observations` (migration 0059). **Now CONSUMED by analytics** (no longer "unconsumed"): `GET /v1/admin/worktier/distribution` (`main.go:1366`, `authed.Get` + `requireAdmin` → 401 — **admin-gated, NOT economy-gated**, matching capture's exempt posture) reads `worktier.Store.Aggregate` (per-workspace, sliced by model; `workspace_id` REQUIRED → 400, no cross-tenant mode). real-PG test: **yes** — `cmd/lens/worktier_analytics_handler_test.go` (#235 `3396781`: tenant isolation, admin-gate no-leak, required-param, window) + the `worktier` mint-free import-guard. The **routing-Advisor tier-conditioning** consumer (conditions routing on the tier) remains a separate future PR.

### A8 · Guardrails
`LENS_GUARDRAILS_ENABLED` **false** (config.go:632) gates **only the OUTPUT stage**; **input guardrails run unconditionally** (`internal/guardrails/engine.go`). Input = **BUILT-&-ON** (default redact PII / block injection); Output = **BUILT-INERT** (default off; even on, block actions are opt-in/observe). Not economy-killswitched. real-PG test: not found.

### A9 · U6 verified-floor + per-identity rate cap (the mint chokepoint)
**BUILT-&-ON** — enforced at the ledger kernel for **every** mint type; **NOT** killswitched (safety).
| Guard | Code | Backing |
|---|---|---|
| Verified-floor `MayEarn` | `internal/earnverify/verify.go:33` (`earn_verified=true` **OR** completed `lxc_purchase>0`) | migration 0057 (`earn_verified`) + 0054 (`lxc_purchases`) |
| Rate cap `checkMintRateCap` | `internal/mining/mint_gate.go:157`; `LENS_MINT_RATE_CAP_LENS_24H` **1000** (config.go:873) | index migration 0058 |
| Chokepoint | both kernels call both gates: `CreditHeldTx → heldInner → verifyEarn + checkMintRateCap` (`held_ledger.go`); `applyTx` (`cache_mining.go`) likewise | — |
| real-PG test | **yes** (`internal/mining/u6_integration_test.go`, `internal/earnverify/verify_integration_test.go`) | — |

### A10 · Closed-test trial config — **BUILT-&-ON** (committed)
`docker-compose.trial.yaml` turns the economy on for a closed internal test (internal valueless ledger, Stripe **test mode**, reversible). Flags set `true`: `LENS_PATTERN_MINING/CAPTURE/EARNING_ENABLED`, `LENS_CACHE_POOLABLE_ENABLED`, `LENS_POOL_ROYALTY_MINTING_ENABLED`, `LENS_POVI_MINTING_ENABLED`, `LENS_WORKTIER_ENABLED`, `LENS_GUARDRAILS_ENABLED`, `LENS_QUALITY_AUTO_RETRY`, `LENS_ROI_INCLUDE_ENGINEER_BREAKDOWN`; tunables `LENS_POOL_HOLDBACK_WINDOW=30s`, `LENS_PATTERN_EARN_CAP_PER_WORKSPACE=3`. Overlay `docker-compose.trial-distill.yaml` adds `LENS_DISTILL_POOLABLE_ENABLED=true`. `LENS_ECONOMY_ENABLED` left unset → default true; `LENS_BILLING_ENABLED` unset → test mode. Bring-up: `docs/closed-test-economy.md`.

### A11 · Royalty observability — admin-gated read surfaces + the automatic sweep
Read-only forensics over both economies. **Admin-gated (`requireAdmin` → 401) but NOT economy-gated** — registered on `authed.Get` (not `econ.get`), so they survive the `LENS_ECONOMY_ENABLED` kill-switch (forensics during an incident — the economy is likeliest off then). The **mutation** endpoints (adjudicate) stay economy-gated. All reader seams are Query/QueryRow-only (type-level no-write). The adversarial admin-gate (non-admin + unauthenticated → 401, no data leaked) is real-PG tested on every endpoint.

| Surface | Status | Endpoint / wiring (file:line) | Reader | real-PG | Last SHA |
|---|---|---|---|---|---|
| Cache detect/resolve/margin | BUILT-&-ON | `GET /v1/admin/pool-royalty/{detect,resolve,margin}` (`main.go:1341-1343`) | `cmd/lens/pool_royalty_observability_handlers.go` over `detector.go`/`resolver.go`/`margin.go` | yes (`..._test.go` + admin-gate) | #227 `1024f15` |
| Distill detect/margin | BUILT-&-ON | `GET /v1/admin/distill-royalty/{detect,margin}` (`main.go:1353,1355`) | `cmd/lens/distill_royalty_observability_handlers.go` | yes | #228 `fc2eca8` |
| Distill resolve | BUILT-&-ON | `GET /v1/admin/distill-royalty/resolve` (`main.go:1354`) | `distill_resolver.go` `DistillResolver` (volume→`content_swarm`, self_dealing→`pair_coarse`; no similarity; held-only) | yes (`distill_resolver_integration_test.go`) | #229 `5bbc81d` |
| **Detector SWEEP** (the smoke detector) | **BUILT-&-ON** (economy-gated) | leader-elected `"royalty-detector-sweep"` ttl 30s (`main.go:707`); runs iff `EconomyEnabled && DetectorSweepEnabled` (`main.go:708`) | `detector_sweep.go` `DetectorSweep` + `findings_writer.go` (append-only `INSERT … ON CONFLICT (identity_key) DO NOTHING`) | yes (`detector_sweep_integration_test.go`) | #230 `11aabe8` |

- **Findings sink:** migration **0065** `royalty_detector_findings` (append-only; `UNIQUE(identity_key)` dedups across sweeps). Metrics gauge `RoyaltyDetectorFlagged{economy,detector}` (`metrics.go:163`, `SetRoyaltyDetectorFlagged:439`; alert on > 0).
- **Never-auto-act (structural):** `DetectorSweep` holds only the read-only detectors + a `Record`-only sink; `detector_sweep.go` imports no ledger and references no mutation primitive in code — pinned by `TestDetectorSweep_NeverActs_ImportGuard` (AST identifiers, comments excluded) **and** a money-safety test (mint tables byte-identical after a sweep over gaming rows).
- **Single-leader / no double-emit:** `haComps.leader.Run` (Redis lease when HA on; runs `fn` directly single-instance) — `internal/ha/leader_test.go` `TestLeader_FnRunsOnlyOnce`.

### A12 · Annotation-mining + reputation — `internal/mining/annotation_mining.go` + `internal/mining/reputation.go`
Stake-to-annotate proof-of-useful-work: annotators stake LENS, review response pairs, and earn on consensus. **Economy-gated** (no own flag — the routes are `econ.get/econ.post`, 404 when `LENS_ECONOMY_ENABLED=false`); earning runs through the **U6 chokepoint** (§A9: `CreditTx → verifyEarn + checkMintRateCap`). Reputation (PR1–3, #232/#233/#234) is **money-decoupled by construction** — it gates task ACCESS + display, never earning.

| Component | Status | Gating | file:line | Migration | real-PG | Last SHA |
|---|---|---|---|---|---|---|
| Annotation earn (base + high-agreement bonus) | BUILT-&-ON (economy-gated) | EconomyEnabled; U6 floor | `annotation_mining.go:368` `CreditTx`; earn = `AnnotationBaseReward 0.100` + `HighAgreementBonus 0.050` (agreement ≥0.80 + ≥3 others); stake `StakeRequirement 10.0`; `TypeAnnotationMine` | 0022 `annotation_tasks`/`annotations`/`annotator_stakes` | yes (`reputation_integration_test.go` earning-invariance) | — |
| Reputation score (event-sourced) | BUILT-&-ON | EconomyEnabled (events only via the sweep) | `reputation.go:86` `reputationScore` = `clamp(0.5 + SUM(delta), 0, 1)`, baseline 0.5 | **0066** `reputation_events` (append-only; `BEFORE UPDATE/DELETE` trigger ⇒ immutable at the DB level) | yes (`reputation_integration_test.go`) | b38369b |
| Access-floor gate (the live consumer) | BUILT-&-ON | EconomyEnabled | `annotation_mining.go:215` `GetPendingTask` returns no task when score `< AccessFloor 0.35` (`reputation.go:34`) — below baseline, so a new / dormant-decayed annotator is **never benched** | 0066 | yes (`reputation_gate_integration_test.go`) | 555d102 |
| Admin re-entry (reset) | BUILT-&-ON (**admin-gated, NOT economy-gated**) | `requireAdmin` → 401 | `POST /v1/admin/annotation-reputation/reset` (`main.go:1373`) → `reputation.go:108` `Reset` APPENDS an `admin_reset` event ⇒ restores baseline (never UPDATE/DELETE) | 0066 | yes (`...reset_handler_test.go` admin-gate, no-leak) | 555d102 |
| Dormancy decay | BUILT-&-ON | EconomyEnabled | `reputation.go:301` `DecayDormant` — earned reputation decays `ReputationDecayRate 0.01`/day toward baseline after `DormancyDays 7`; clamped so it **FLOORS AT baseline** (never below) | 0066 | yes (`reputation_decay_integration_test.go`) | c68a573 |
| Resolution + decay sweep | BUILT-&-ON (economy-gated) | leader-elected `"annotation-reputation-resolution"` ttl 30s (`main.go:811`); runs iff `EconomyEnabled` | `reputation.go:372` `StartScheduler` runs `ResolveExpiredTasks` (`:156`, final-consensus agreement → `agreement_outcome` events) **+** `DecayDormant` per tick | 0066 | yes | c68a573 |
| Display (real computed score) | BUILT-&-ON | EconomyEnabled | `annotation_mining.go:654` `GetAnnotatorStats` returns the computed score (was a hardcoded `1.0`) | 0066 | yes | b38369b |

> **Money-decoupled (doctrine, structurally enforced):** all reputation code lives in `reputation.go`; the earning path (`SubmitAnnotation` :263 → `CreditTx` :368 = base + bonus) references **no** reputation symbol — pinned by `TestReputation_MoneyBoundary_ASTGuard` (`reputation_integration_test.go:223`, AST identifiers) + an earning-invariance test (two annotators with opposite reputations → byte-identical earning). The event log is append-only at the DB level (0066 trigger rejects UPDATE/DELETE); the resolution sweep, decay, and admin reset all only INSERT.

---

## §B — Every economy flag: default + what flipping it does

All booleans below are `parseBoolEnv` (**false** when unset) unless noted, and are **force-false** by `LENS_ECONOMY_ENABLED=false` unless marked **(exempt)**.

| Flag | Default | config.go | Flipping ON does… |
|---|---|---|---|
| `LENS_ECONOMY_ENABLED` | **TRUE** | :1130 | Master switch. Setting **false** force-offs the 10 economy gates below the line; the economy route surface is also unregistered in `main.go`. |
| `LENS_POOL_ROYALTY_MINTING_ENABLED` | false | :624 | Arms the **cache + distill** reuse-royalty mint (held → finalized). No effect without pooling consent + rows. |
| `LENS_POVI_MINTING_ENABLED` | false | :622 | Lets a verified, **staked** node's receipt mint LENS. Idle without a running `cmd/node`. |
| `LENS_PATTERN_MINING_ENABLED` | false | :621 | Opens the per-workspace pattern opt-in route (503 otherwise). |
| `LENS_PATTERN_CAPTURE_ENABLED` | false | :627 | Post-serve, mint-free pattern observation capture (routing corpus). |
| `LENS_PATTERN_EARNING_ENABLED` | false | :628 | The pattern **earn** path (mints, rarity-bound + cap + idempotent). |
| `LENS_TRUSTFUL_COMPUTE_MINT_ENABLED` | false | :964 | Would arm the legacy trust-mint — **dead** (`NotifyServed` has no caller). |
| `LENS_CACHE_SHARING_ENABLED` | false | :618 | Cross-tenant cache sharing primitive. |
| `LENS_CACHE_POOLABLE_ENABLED` | false | :619 | Cross-tenant cache pooling (cache-royalty substrate). |
| `LENS_DISTILL_POOLABLE_ENABLED` | false | :620 | Cross-tenant OCR pooling (distill-royalty substrate); still needs per-WS dual consent. |
| `LENS_ROUTING_INTELLIGENCE_ENABLED` | false | :635 | Pattern-aggregate auto-route model selection (auto requests only). |
| `LENS_WORKTIER_ENABLED` | false | :633 | **(exempt)** Descriptive work-tier capture (mint-free); the admin analytics endpoint `GET /v1/admin/worktier/distribution` reads the aggregate (§A7). |
| `LENS_GUARDRAILS_ENABLED` | false | :632 | **(exempt)** Enables the **output**-stage guardrails (input always runs). |
| `LENS_BILLING_ENABLED` | false | :637 | **(exempt)** Stripe checkout/webhook/refund. Requires both Stripe keys. |
| `LENS_LXC_GATING_ENABLED` | false | :626 | **(exempt)** Pre-serve 402 when LXC exhausted (inert unless shadow also on). |
| `LENS_LXC_SHADOW_SPEND_ENABLED` | false | :625 | **(exempt)** Post-serve observational LXC debit. |

### Numeric / non-boolean economy knobs
| Env | Default | config.go | Effect |
|---|---|---|---|
| `LENS_POOL_ROYALTY_SHARE` | **0.5** | :939 | Contributor share `s` of avoided-COGS (cache + distill); clamped [0,1]. |
| `LENS_POOL_HOLDBACK_WINDOW` | **72h** | :881 | Held→final settlement delay. |
| `LENS_MINT_RATE_CAP_LENS_24H` | **1000** | :873 | U6 per-identity rate cap (0 disables). **(exempt)** safety. |
| `LENS_POOL_MINT_CAP_PER_PAIR` / `_PER_ENTRY` | **0/0** (off) | :803 | Cache per-pair / per-entry mint caps. |
| `LENS_DISTILL_MINT_CAP_PER_PAIR` / `_PER_CONTENT` | **0/0** (off) | :828 | Distill per-pair / per-content mint caps (separate budget). |
| `LENS_DISTILL_MINT_CAP_WINDOW` | **24h** | :844 | Distill cap rolling window. |
| `LENS_POVI_MIN_STAKE` | **100.0** | :779 | Min LENS a node stakes to be mint-eligible. |

### Detector-sweep knobs (observability — gated by `EconomyEnabled` at the wiring, NOT in the force-off block)
| Env | Default | config.go | Flipping / setting does… |
|---|---|---|---|
| `LENS_DETECTOR_SWEEP_ENABLED` | **TRUE** | :327 (set :951) | The scheduled cache+distill detector sweep. Net gate = `EconomyEnabled && DetectorSweepEnabled`. **Default-true** so the smoke detector accompanies minting automatically; setting it **false** is the manual off-switch. |
| `LENS_DETECTOR_SWEEP_INTERVAL` | **1h** | :328 (set :955) | Sweep cadence (the inner `StartScheduler` tick; lease ttl stays 30s). |
| `LENS_DETECTOR_SWEEP_WINDOW` | **24h** | :329 (set :963) | The rolling detection window the sweep passes to the detectors. |

---

## §C — Discrepancies (code wins)

1. **Stale "no production caller until S4" comments.** `internal/mining/pattern_mining.go:211-212` and `cmd/lens/main.go:790-791` claim the pattern earn path has no production caller. **Code contradicts this:** `SetPatternEarn` is wired (`main.go:849`) and the serve path calls `earnPattern → RecordPattern` (`proxy.go:1331`). Accurate state: the earn path is **gated-inert** behind `LENS_PATTERN_EARNING_ENABLED` (default off), **not** caller-absent.
2. **Stale config.go line citations in docs/compose.** `docs/closed-test-economy.md` and the trial-compose comments cite older line numbers (from SHA `ac6dc82`, e.g. 613/611/845/1094). The **current** lines are those in this manifest (e.g. mint flag :624, PoVI :622, holdback :881, economy :1130). The flag **names + defaults** are unchanged; only the line numbers drifted.
3. **RESOLVED (was: "the detectors are not wired into `cmd/`").** The cache + distill `detect`/`resolve`/`margin` readers are now wired BOTH as on-demand admin endpoints (#227 `1024f15` cache, #228 `fc2eca8` + #229 `5bbc81d` distill) AND an automatic leader-elected sweep that records flagged findings (#230 `11aabe8`) — see **§A11**. The prior "exists-but-not-wired" gap is closed; the distill resolver (the one missing parity piece) was built in #229.
4. **node-earning is PARTIAL, not "no substrate."** The `cmd/node` daemon + PoVI receipt→mint path exist and are wired; what's missing is a **running node** for the verifier↔prover round-trip — runtime substrate, not code.
5. **RESOLVED (was: "routing-intelligence has no real-PG test").** `internal/mining/aggregate_cohorts_integration_test.go` (#236 `6d36725`) now EXECUTES `aggregateCohortsSQL` against real `routing_patterns` — the opt-out **privacy exclusion** is a proven DB behavior, not just a string assertion (`aggregate_cohorts_test.go`). The Advisor's in-memory ranking/floors stay unit-tested (`routing_test.go`). See §A6.
6. **RESOLVED (was: "WorkTier captured-but-unconsumed / nothing consumes the tier yet").** The analytics endpoint `GET /v1/admin/worktier/distribution` (#235 `3396781`) consumes `worktier.Store.Aggregate` — admin-gated, tenant-scoped, money-decoupled. Capture stays mint-free + kill-switch-exempt; only the routing-Advisor tier-conditioning consumer remains future. See §A7.
7. **Annotation-mining + reputation was absent from this manifest; now §A12.** The annotation track + the event-sourced reputation system (score / access-floor gate / dormancy decay / admin reset, migration 0066) are money-decoupled (AST-guarded) and were merged after the prior regen (#232–#234). Added in this pass.
