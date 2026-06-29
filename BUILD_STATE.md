# BUILD_STATE.md — Talyvor Lens canonical build-state manifest

**Single source of truth for "what is built," derived from the actual code at the SHA below — never from the roadmap, notes, or assumptions.** Regenerated (never hand-edited) whenever it goes stale.

- **Derived from main:** `c4742ab`
- **Latest migration:** `0068_proof_of_benchmark.sql`
- **Config:** all `config.go:LINE` citations are `internal/config/config.go`.
- **Method:** every cell was grep'd / read from code. Where code and a note/roadmap disagree, **the code wins** — see [§C Discrepancies](#c-discrepancies-code-wins).

## Status legend
- **BUILT-&-ON** — built and active by default (no flag, or its flag/default is on).
- **BUILT-INERT** — fully built but behind a **default-off** flag (or no rows exist until one flips). The default posture.
- **PARTIAL** — built but cannot operate as-is (needs substrate that doesn't exist single-box).
- **ABSENT** — no code (or a deliberately omitted analog).

---

## The master kill-switch (read this first)

`LENS_ECONOMY_ENABLED` — **default TRUE** (`config.go:1198` sets `c.EconomyEnabled = true`, overridden only if the env var is explicitly set, `:1199`). When **false**, the force-off block force-sets **11** flags false regardless of their own env:

`PatternMiningEnabled · PatternCaptureEnabled · PatternEarningEnabled · PoolRoyaltyMintingEnabled · POVIMintingEnabled · TrustfulComputeMintEnabled · CacheSharingEnabled · CachePoolableEnabled · DistillPoolableEnabled · RoutingIntelligenceEnabled · RoutingTierCohortsEnabled`

**Deliberately NOT force-offed** (documented exceptions): `LXCGatingEnabled` / `LXCShadowSpendEnabled` (fiat-pegged, not the token economy), the **U6 floor + rate cap** (safety restrictions), `WorkTierEnabled` / `GuardrailsEnabled` (non-economic), and the three **measurement/routing/capability** flags added since the last regen — `NodeAutoRouteEnabled`, `ReputationBondedMintingEnabled`, `ProofOfBenchmarkEnabled` (each only ever *reduces/blocks/redistributes* a mint or routes traffic; none CREATES a mint, so none belongs in the force-off block). The manifest test `cmd/lens/economy_killswitch_test.go` asserts **`len(checks) == 11`** (`:64-65`) for the force-off set and that LXC stays wired; the `economyGateEnv` adversarial input list is **12** env vars (the 11 minus tier-cohorts, plus the 2 LXC force-ON inputs — a different count by construction).

---

## §A — Component build-state

### A1 · PoVI + node-earning
| Component | Status | Gating flag + default | Key files (file:line) | Migration | real-PG test | Last SHA |
|---|---|---|---|---|---|---|
| PoVI mint kernel | BUILT-INERT | `LENS_POVI_MINTING_ENABLED` **false** (config.go:662) | `internal/povi/mint.go:50` `MintFromReceipt` (gate `:51`) | 0019 ledger | indirect | (povi pkg) |
| Receipts (ed25519) + Merkle | BUILT-&-ON | none (crypto primitive) | `receipt.go:109/118`, `merkle.go:78/96/128` | — | unit | — |
| Stake + **real Slash** | BUILT-&-ON | `LENS_POVI_MIN_STAKE` **100.0** (config.go:823) | `internal/povi/stakes.go:303` `Slash`; `internal/mining/stake_ledger.go:94` | 0032 `povi_stakes` | **yes** (`stakes_concurrency_integration_test.go`) | — |
| Challenge-and-slash + scheduler | BUILT-INERT | `LENS_POVI_CHALLENGE_RATE` (no-op at 0); needs a live node | `challenge.go:234` `Challenge`, `challenge_scheduler.go` | 0033 `povi_challenges` | indirect | — |
| Receipts processor + HTTP route | BUILT-INERT | reads `cfg.POVIMintingEnabled` | `internal/povi/processor.go:80` `Process`; route `cmd/lens/main.go:3162` → `.Process` | 0031 `povi_receipts` | **yes** (`node_harness_integration_test.go` #240) | 18eeb68 |
| **node-earning** (`cmd/node` daemon) | **BUILT-&-ON in closed-test** (#240–#243) | the PoVI flag + a running node | `cmd/node/main.go` (daemon: `/inference`, `/challenge`); closed-test harness `internal/povi/node_harness_integration_test.go` | — | yes | 18eeb68 |
| Legacy trust-mint (`RecordServedRequest`) | **ABSENT (dead path)** | `LENS_TRUSTFUL_COMPUTE_MINT_ENABLED` **false** (config.go:1031) | `internal/mining/compute_mining.go` via `Router.NotifyServed` (no caller) | 0020 | — | — |

> **node-earning is no longer "needs substrate."** PR #240 (`18eeb68`) landed the real-PG closed-test harness proving register → grant → stake → vouch → receipt → `poviProcessor.Process` mints (and the U6 floor zero-mints an unverified node). The `cmd/node` daemon ships in the image (Dockerfile builds `./cmd/node`) and the trial overlay runs it (§A10). **`NotifyServed` still has ZERO callers** → the legacy trust-mint path stays dead.

### A2 · Cache (Pool-B) royalty — `internal/poolroyalty/` (11 components)
Mint components gate on `LENS_POOL_ROYALTY_MINTING_ENABLED` **false** (config.go:664); `MintServedHit` re-checks per call. The held credit (`TypePoolRoyaltyHeld`) is now also a **reputation-bonded** mint type (§A13) — when `LENS_REPUTATION_BONDED_MINTING_ENABLED` is on, `f(R)` scales it at the chokepoint. All `*_integration_test.go` gate on `LENS_TEST_DATABASE_URL`.
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

### A3 · Distill reuse-royalty — `internal/poolroyalty/distill_*` + `internal/proxy/distill_integration.go`
Full parity with the cache royalty's anti-gaming + observability set (detector, resolver, margin, revoke/adjudication — all wired, §A11). `LENS_DISTILL_POOLABLE_ENABLED` **false** (config.go:660) + per-WS dual consent; the mint shares `LENS_POOL_ROYALTY_MINTING_ENABLED` **false** (config.go:664) and `TypePoolRoyaltyHeld` (so it is also reputation-bonded, §A13). Key files: `distill_minter.go` (0062 `distill_royalty_mints`, request_id UNIQUE), `distill_detector.go` (no similarity — exact `content_hash`), `distill_resolver.go` (volume→`content_swarm`, self_dealing→`pair_coarse`), `distill_margin.go` (0064 view), `0061` basis / `0063` adjudications. Avoided-COGS basis (`internal/distillattrib/store.go`) is BUILT-&-ON descriptive (no mint).

### A4 · Pattern economy — `internal/mining/pattern_mining.go` + `internal/proxy/pattern_*`
| Component | Status | Gating flag + default | file:line | Migration |
|---|---|---|---|---|
| S1 rarity bound | BUILT-&-ON (always applied) | none | `pattern_mining.go:307` `ScoreRarity` | — |
| S2 per-window earn cap | BUILT-&-ON | `LENS_PATTERN_EARN_CAP_PER_WORKSPACE` **50000**, window **24h** | `pattern_mining.go:370` | — |
| S3 idempotency claim | BUILT-&-ON (structural) | none | claim-first write | 0049 `pattern_mine_credits` (UNIQUE `(request_id, workspace_id)`) |
| S4 earn wire-up | BUILT-INERT | `LENS_PATTERN_EARNING_ENABLED` **false** (config.go:668) | `internal/proxy/pattern_earn.go` → `proxy.go` | (uses 0049) |
| Capture path | BUILT-INERT | `LENS_PATTERN_CAPTURE_ENABLED` **false** (config.go:667) | `internal/proxy/pattern_capture.go` | — |
| Mining opt-in route | BUILT-INERT | `LENS_PATTERN_MINING_ENABLED` **false** (config.go:661) | route in `main.go` | — |

### A5 · LXC billing (fiat — independent of the token economy)
**BUILT-INERT** — full Stripe checkout/webhook/refund/idempotency, default off. `LENS_BILLING_ENABLED` **false** (config.go:681); **requires BOTH** `LENS_STRIPE_SECRET_KEY` + `LENS_STRIPE_WEBHOOK_SECRET` or startup fails. Table `lxc_purchases` (migration **0054**). LXC gating/shadow: `LENS_LXC_GATING_ENABLED` **false** (config.go:666) · `LENS_LXC_SHADOW_SPEND_ENABLED` **false** (config.go:665) — **NOT** economy-killswitched. real-PG test: **yes** (`internal/billing/billing_integration_test.go`).

### A6 · Routing-intelligence + tier-cohorts
**BUILT-INERT** · `LENS_ROUTING_INTELLIGENCE_ENABLED` **false** (config.go:675), **in the force-off block**. Pattern aggregates → auto-route MODEL selection on `auto` requests only (`internal/routing/routing.go`); in-memory `Recommend`, corpus loaded on a timer (`aggregateCohortsSQL`). real-PG: `aggregate_cohorts_integration_test.go` (privacy exclusion). **Tier-cohorts (#238, Shape 3 + Shape 1):** `LENS_ROUTING_TIER_COHORTS_ENABLED` **false** (config.go:676), **in the force-off block**; refines the Advisor by complexity tier (`worktier.ComplexityBucketFor` → `routing_patterns.complexity_bucket`, migration **0067**). Only meaningful with routing-intelligence on; off ⇒ routing byte-identical.

### A7 · WorkTier
**BUILT-INERT** (capture) · `LENS_WORKTIER_ENABLED` **false** (config.go:673), **NOT** in the force-off block (descriptive ⇒ off=safe). Mint-free by construction (`internal/worktier/worktier.go`, import-guard test). Write-only post-flush to `work_tier_observations` (migration 0059). **Consumed by:** (a) analytics `GET /v1/admin/worktier/distribution` (admin-gated, money-decoupled); (b) the routing-Advisor **tier-conditioning** (Shape 1 downgrade-eligibility gate, #198 — subtractive, rides the RoutingIntelligence gate) + Shape 3 tier-cohorts (#238, §A6).

### A8 · Guardrails
`LENS_GUARDRAILS_ENABLED` **false** (config.go:672) gates **only the OUTPUT stage**; **input guardrails run unconditionally** (`internal/guardrails/engine.go`). Input = BUILT-&-ON (redact PII / block injection); Output = BUILT-INERT. Not economy-killswitched.

### A9 · U6 verified-floor + per-identity rate cap (the mint chokepoint)
**BUILT-&-ON** — enforced at the ledger kernel for **every** mint type; **NOT** killswitched (safety).
| Guard | Code | Backing |
|---|---|---|
| Verified-floor `MayEarn` | `internal/earnverify/verify.go` (`earn_verified=true` **OR** completed `lxc_purchase>0`); checked by `verifyEarn` (`mint_gate.go:106`) | migration 0057 (`earn_verified`) + 0054 (`lxc_purchases`) |
| Rate cap `checkMintRateCap` | `mint_gate.go:229`; wired `main.go:615` `SetMintRateCap`; `LENS_MINT_RATE_CAP_LENS_24H` **1000** (config.go:917) | index migration 0058 |
| Reputation bond (P1 #9) | `mint_gate.go:169` `reputationBondedAmount` — an ADDITIVE downstream constraint on bonded mint types (§A13) | reuses 0066 |
| Chokepoint | both kernels run verifyEarn → reputation bond → rate cap: `CreditHeldTx → heldInner` (`held_ledger.go:139`) and `Credit → applyTx` (`cache_mining.go:221`) | — |
| real-PG test | **yes** (`u6_integration_test.go`, `verify_integration_test.go`, `seed_zeromint_integration_test.go`) | — |

### A10 · Closed-test trial config — **BUILT-&-ON** (committed, ACTIVATED #241/#243)
`docker-compose.trial.yaml` runs the economy on for a closed internal test (internal valueless ledger, Stripe **test mode**, reversible). Flags set `true` include the pattern trio, `LENS_CACHE_POOLABLE_ENABLED`, `LENS_POOL_ROYALTY_MINTING_ENABLED`, **`LENS_POVI_MINTING_ENABLED`** (#241), **`LENS_ROUTING_INTELLIGENCE_ENABLED` + `LENS_ROUTING_TIER_COHORTS_ENABLED` + `LENS_CACHE_SHARING_ENABLED`** (#241), **`LENS_NODE_AUTOROUTE_ENABLED`** (#243), `LENS_WORKTIER/GUARDRAILS/QUALITY_AUTO_RETRY`; tunables `LENS_POOL_HOLDBACK_WINDOW=30s`, `LENS_PATTERN_EARN_CAP_PER_WORKSPACE=3`; fixed `LENS_POVI_CHALLENGE_KEY`. Overlay `docker-compose.trial-distill.yaml` adds `LENS_DISTILL_POOLABLE_ENABLED=true`. A `node` service (Dockerfile builds `./cmd/node`) registers + serves. `LENS_ECONOMY_ENABLED` unset → default true. Bring-up: `docs/closed-test-economy.md`.

### A11 · Royalty observability — admin-gated read surfaces + the automatic sweep
Read-only forensics over both economies. **Admin-gated (`requireAdmin` → 401) but NOT economy-gated** (registered on `authed.Get`, survive the kill-switch). Cache + distill `detect`/`resolve`/`margin` admin endpoints (`main.go:1341-1355`); the leader-elected **detector sweep** (`"royalty-detector-sweep"`, runs iff `EconomyEnabled && DetectorSweepEnabled`, default-true) records to **0065** `royalty_detector_findings` (append-only, `UNIQUE(identity_key)`). Never-auto-act is structural (import-guard + money-safety test).

### A12 · Annotation-mining + reputation — `internal/mining/annotation_mining.go` + `internal/mining/reputation.go`
Stake-to-annotate proof-of-useful-work: annotators stake LENS, review response pairs, earn on consensus. **Economy-gated** (routes are `econ.*`); earning runs through the **U6 chokepoint** (§A9). Reputation (event-sourced, `reputation.go:86` `reputationScore = clamp(0.5 + SUM(delta))`, baseline 0.5, `AccessFloor 0.35`) lives in **0066** `reputation_events` (append-only; DB trigger rejects UPDATE/DELETE). Score / access-floor gate / dormancy decay (`ReputationDecayRate 0.01`/day after `DormancyDays 7`, floors at baseline) / admin reset / resolution+decay sweep — all only INSERT.

> **Money-decoupling — now NUANCED by P1 #9.** Reputation is **money-decoupled from ANNOTATION earning** (the AST guard `TestReputation_MoneyBoundary_ASTGuard` pins that `SubmitAnnotation → CreditTx` references no reputation symbol — still green). But reputation is **DELIBERATELY coupled to PoVI-receipt + pool-royalty-held MINTING** via the #9 bond (§A13), at a different code path (the ledger chokepoint, not the annotation earning path). The two coexist: annotation earning ignores reputation; PoVI/royalty mints are reputation-scaled when the #9 flag is on.

### A13 · Reputation-bonded minting (P1 #9) — PR #244 (`4349640`)
**BUILT-INERT** · `LENS_REPUTATION_BONDED_MINTING_ENABLED` **false** (config.go:678, field `:425`), **NOT** in the force-off block (a mint-*reducer*, not an enabler). **No migration** (reuses 0066 `reputation_events`).
| Component | Status | file:line |
|---|---|---|
| `f(R)` gate + scale at the chokepoint | BUILT-INERT | `mint_gate.go:169` `reputationBondedAmount`; gate `:126` `ErrReputationFloor`; `f(R)=clamp01((R−0.35)/(0.50−0.35))` (0 below floor, 1.0 at/above baseline — never amplifies) |
| Bonded-type allow-list | BUILT | `mint_gate.go:132` `isReputationBondedType` = `{receipt_mine_provisional, pool_royalty_held}` ONLY (excludes annotation/cache/compute/embedding/pattern) |
| Applied at both kernels | BUILT | `cache_mining.go:221` (Credit/PoVI), `held_ledger.go:139` (CreditHeldTx/royalty) — downstream of `verifyEarn`, composes-not-bypasses U6 |
| `slash` signal (δ −0.10) | BUILT-INERT | `internal/povi/stakes.go:117` `SlashReputationDelta`; appended IN the slash tx `:355` (atomic with the stake burn) |
| `challenge_pass` signal (δ +0.02) | BUILT-INERT | `internal/povi/challenge.go:192` `ChallengePassReputationDelta`; appended best-effort `:293` |

> NO-LOOP holds: R moves only via `agreement_outcome` / `decay` / `admin_reset` / `slash` / `challenge_pass` — never mint volume. real-PG: `seed_zeromint`-style proof in `internal/mining/reputation_bonded_minting_integration_test.go` (+ the slash→R→mint e2e in `internal/povi/slash_reputation_integration_test.go`). **Open follow-up:** the per-mint `SUM(delta)` fold is indexed but O(events-per-workspace); a materialized current-R is logged.

### A14 · Proof-of-benchmark (P1 #10) — PRs #245/#246/#247
Challenge-verified per-node QUALITY → routing weight → emergent PoVI earning. **No new mint.** `LENS_PROOF_OF_BENCHMARK_ENABLED` **false** (config.go:679, field `:432`), **NOT** in the force-off block. Migration **0068** (`benchmark_eval_items` verifier-private pool — no workspace_id; `benchmark_node_scores`; `benchmark_probes` UNIQUE(node_id,item_id) never-reuse).
| Component | Status | file:line | PR/SHA |
|---|---|---|---|
| Verifier-private pool + scheduler (crypto/rand draw, never-reuse, node-blind payload, `eval.StaticScore`) | BUILT-INERT | `internal/benchprobe/` (`store.go`, `scheduler.go`, `benchprobe.go`) | #245 `a3e29cd` |
| Operator seed tool | BUILT | `cmd/lens-benchseed/main.go` | #245 |
| Live `/inference` delivery (#242 node-auth token; injected signer keeps benchprobe povi-free) | BUILT-INERT | `internal/benchprobe/delivery.go` `HTTPDelivery` | #246 `508c489` |
| **Probe-mint suppression** (the one money-path touch) | BUILT-INERT | `internal/povi/processor.go:51` `SetProbeChecker`, `:137` `case probe:` (record-but-skip-mint, point lookup `benchprobe.Store.IsProbe` `store.go:119`) | #246 |
| Routing-weight consumer (ε-greedy 0.15, Bayesian-shrink k=5, per-strategy compose; sync loop, zero per-request DB) | BUILT-INERT | `internal/localrouter/multi.go:488` `SetQualityEnabled`, `:514` `selectQualityWeighted`, `:594` `StartQualitySync` | #247 `abd1572` |

> **Documented residual (honest-node guarantee):** the receipt `request_id` is node-asserted (not gateway-bound), so the suppression is robust for honest nodes; a malicious node can bypass via a non-probe request_id — the SAME pre-existing receipt-fabrication capability (no new surface from probes), deterred by challenge-and-slash + stake + rate-cap + the #9 bond. The **gateway-bound-request_id** fix is a tracked **pre-public-mint gate**, separate from #10. NO-LOOP intact (import-guard: benchprobe + localrouter reference no ledger/mint symbol; the score is from `staticScore`, never mint volume).

### A15 · L·seed warm-start cache — PR #239 (`b598493`-era; merged)
**BUILT-INERT (zero-mint by construction)** · operator action, no flag. `cmd/lens-seed` + `internal/seedcache/` write Talyvor-OWNED warm-start cache entries (exact + semantic + distill-OCR) so a fresh deploy serves hits on day one. Owner is the dedicated `economy.TalyvorSeedWorkspace = "talyvor-seed"` — **never earn_verified, never an lxc_purchase**, so `earnverify.MayEarn` is false → both royalty mint paths roll back at the shared `verifyEarn` chokepoint → seeds provably **mint nothing**. Written only via the public store methods (no raw SQL); **no migration**, no new mint surface. real-PG zero-mint + contrast proof in `internal/poolroyalty/seed_zeromint_integration_test.go`.

### A16 · Gateway node auto-route — PRs #242 (`f210a2f`) + #243 (`a56dca6`)
**BUILT-INERT** · `LENS_NODE_AUTOROUTE_ENABLED` **false** (config.go:677, field `:416`), **NOT** in the force-off block (routing, not a mint gate). When on, normal `/v1/proxy/*` traffic auto-routes to a registered node (`internal/proxy/proxy.go` `tryNodeRouting`, `SelectEndpoint(StrategyLeastLoaded)`); the node serves + auto-submits its own receipt → minting stays gated downstream by the U6 chokepoint. Authz = a gateway-signed **node-auth token** reusing the existing challenge keypair (`povi.SignNodeAuthToken` / node verifies with its pinned challenge pubkey — `internal/povi/nodeauth.go`); no new secret-at-rest, no migration. Off ⇒ serve path byte-identical (legacy `localrouter.New(cfg.OllamaURL)`). Activated in the trial overlay (§A10, #243). real-PG + live-stack proof in the PoVI harness arc.

### A17 · Proof-of-Improvement rail, piece 1 — pluggable reward anchor — PR #248 (`c4742ab`)
**BUILT — valuation-only generalization; no chokepoint touched, no migration, no reachable new mint surface.** Generalizes the proof-of-savings (#2) minter so the reward **anchor** (how a measured gain is priced) is pluggable, seeding a reusable **Proof-of-Improvement** primitive: a contributor measurably improves a SHARED Talyvor artifact → mint proportional to the MEASURED gain → through the existing U6/held-ledger chokepoint.
| Component | Status | file:line |
|---|---|---|
| `Anchor` interface (`Value(GainInput) float64`, `Kind() string`) — pure valuation, never touches the ledger | BUILT | `internal/poolroyalty/anchor.go` |
| `CostAnchor{Share}` (the default) = `Share × AvoidedCOGSUSD` — **byte-identical** to #2 | BUILT-&-ON | both minters build it in the constructor (`minter.go:250`, `distill_minter.go:130`); used at `minter.go:283` / `distill_minter.go:248` |
| `HeldBenchmarkAnchor{rate}` = `rate × clamp01(HeldScore)` (the #10 held-ground-truth pattern) — **rate REQUIRED** (`NewHeldBenchmarkAnchor` rejects 0/neg/NaN/Inf, no default mint) | BUILT, **mechanically test-only** | `anchor.go` `NewHeldBenchmarkAnchor`; reachability AST guard `anchor_test.go` `TestHeldBenchmarkAnchor_TestOnly_NoLiveSelection` fails if `NewHeldBenchmarkAnchor`/`SetAnchor` is called from ANY non-test `.go` |
| NaN/Inf/≤0 amount guard (unchanged) | BUILT-&-ON | `minter.go` / `distill_minter.go` (after the anchor returns `amount`) |
| `SetAnchor` setter (reserved for the future held-benchmark caller) | BUILT, unused live | `minter.go` / `distill_minter.go` |

- **Flag:** `LENS_PROOF_OF_IMPROVEMENT_ENABLED` **false** (config.go field `:440`, parse `:679`-region), **NOT** in the force-off block (a capability that cannot outrun U6). This PR wires no reachable selection — `main.go` injects nothing, the cost default stands — so the flag is byte-identical on or off; on it only emits a startup log.
- **U6 chokepoint UNTOUCHED:** the anchor computes `amount` upstream; `verifyEarn` + reputation bond (#9, §A13) + 1000-LENS/24h rate cap run on `amount` downstream exactly as today (`held_ledger.go`/`mint_gate.go` not edited). **No migration** (`HeldBenchmarkAnchor`'s future score source `benchmark_node_scores` already exists in 0068; this PR reads nothing new).
- **NO-LOOP:** `anchor.go` references no ledger/mining/benchprobe/DB symbol (import-guard `TestAnchor_NoLedgerNoMint_ImportGuard`); the held score is a pure `GainInput` (the anchor reads no DB and writes nothing), so a mint paid on it can never feed the score it prices. Regression oracle: the full `internal/poolroyalty` package (incl. `seed_zeromint`) passes **unchanged** — cost path byte-identical (amount + `royalty_share` JSONB).

> **#4 is REFRAMED.** Recon confirmed federated-learning-as-named has no substrate (no training loop, no gradient/aggregation, all served models external) and the only Talyvor-owned model is parked Phase-6 — so #4 is reframed as the **Proof-of-Improvement** primitive built against the best existing shared artifact (the proof-of-savings cache, #2, the ✅-hard-readout/✅-clean-attribution baseline). Piece 1 (this PR) is the pluggable anchor. Queued next instances: **proof-of-eval-contribution** (held-benchmark anchor + a contributor-improvable eval surface) and **proof-of-routing-prediction**; an eventual Phase-6 owned-model gain would reuse the same rail.

---

## §B — Every economy / feature flag: default + force-off membership (current lines)

All booleans are `parseBoolEnv` (**false** when unset) unless noted; **force-false** by `LENS_ECONOMY_ENABLED=false` unless marked **(exempt)**.

| Flag | Default | parse line | In force-off block? | Flipping ON does… |
|---|---|---|---|---|
| `LENS_ECONOMY_ENABLED` | **TRUE** | :1199 (set :1198) | n/a (the block itself) | Master switch; **false** force-offs the 11 gates below + unregisters the economy route surface. |
| `LENS_POOL_ROYALTY_MINTING_ENABLED` | false | :664 | **yes** | Arms the cache + distill reuse-royalty mint (held → finalized). |
| `LENS_POVI_MINTING_ENABLED` | false | :662 | **yes** | Lets a verified, staked node's receipt mint LENS. |
| `LENS_PATTERN_MINING_ENABLED` | false | :661 | **yes** | Opens the per-workspace pattern opt-in route. |
| `LENS_PATTERN_CAPTURE_ENABLED` | false | :667 | **yes** | Post-serve mint-free pattern capture. |
| `LENS_PATTERN_EARNING_ENABLED` | false | :668 | **yes** | The pattern earn path (mints). |
| `LENS_TRUSTFUL_COMPUTE_MINT_ENABLED` | false | :1031 | **yes** | Legacy trust-mint — dead (`NotifyServed` has no caller). |
| `LENS_CACHE_SHARING_ENABLED` | false | :658 | **yes** | Cross-tenant cache sharing. |
| `LENS_CACHE_POOLABLE_ENABLED` | false | :659 | **yes** | Cross-tenant cache pooling (cache-royalty substrate). |
| `LENS_DISTILL_POOLABLE_ENABLED` | false | :660 | **yes** | Cross-tenant OCR pooling (distill-royalty substrate). |
| `LENS_ROUTING_INTELLIGENCE_ENABLED` | false | :675 | **yes** | Pattern-aggregate auto-route model selection. |
| `LENS_ROUTING_TIER_COHORTS_ENABLED` | false | :676 | **yes** | Tier-conditioned cohorts (#238); needs routing-intelligence on. |
| `LENS_NODE_AUTOROUTE_ENABLED` | false | :677 | **no** | Gateway auto-route to a registered node (§A16). Routing, not a mint. |
| `LENS_REPUTATION_BONDED_MINTING_ENABLED` | false | :678 | **no** | `f(R)` bond on PoVI/royalty mints (§A13). Reduces/blocks, never enables. |
| `LENS_PROOF_OF_BENCHMARK_ENABLED` | false | :679 | **no** | Probe scheduler + quality routing bias + probe-mint suppression (§A14). Measurement/routing. |
| `LENS_PROOF_OF_IMPROVEMENT_ENABLED` | false | field :440 | **no** | Capability to select a non-cost reward anchor in a future eval-contribution mint (§A17). Wires nothing reachable today; cannot outrun U6. |
| `LENS_WORKTIER_ENABLED` | false | :673 | **(exempt)** | Descriptive work-tier capture (mint-free). |
| `LENS_GUARDRAILS_ENABLED` | false | :672 | **(exempt)** | Output-stage guardrails (input always runs). |
| `LENS_QUALITY_AUTO_RETRY` | false | :656 | **(exempt)** | One-shot re-call on low quality (provider COGS). |
| `LENS_BILLING_ENABLED` | false | :681 | **(exempt)** | Stripe checkout/webhook/refund (requires both Stripe keys). |
| `LENS_LXC_GATING_ENABLED` | false | :666 | **(exempt)** | Pre-serve 402 when LXC exhausted. |
| `LENS_LXC_SHADOW_SPEND_ENABLED` | false | :665 | **(exempt)** | Post-serve observational LXC debit. |

### Numeric / non-boolean knobs
| Env | Default | config.go | Effect |
|---|---|---|---|
| `LENS_POOL_ROYALTY_SHARE` | **0.5** | field :335 | Contributor share `s` of avoided-COGS; clamped [0,1]. |
| `LENS_POOL_HOLDBACK_WINDOW` | **72h** | field :308 | Held→final settlement delay. |
| `LENS_MINT_RATE_CAP_LENS_24H` | **1000** | set :917 | U6 per-identity rate cap (0 disables). **(exempt)** safety. |
| `LENS_POVI_MIN_STAKE` | **100.0** | set :823 | Min LENS a node stakes to be mint-eligible. |
| `LENS_DETECTOR_SWEEP_ENABLED` | **TRUE** | set :988 | Scheduled cache+distill detector sweep. Net gate = `EconomyEnabled && DetectorSweepEnabled`. |

---

## §C — Discrepancies (code wins)

1. **Force-off set is 11, not 10.** `RoutingTierCohortsEnabled` (#238) joined the block; `economy_killswitch_test.go:64` asserts `len(checks)==11`. The three flags added since (`NodeAutoRoute`, `ReputationBondedMinting`, `ProofOfBenchmark`) are DELIBERATELY excluded — each only routes / reduces / suppresses, never creates a mint.
2. **Reputation is no longer purely money-decoupled.** P1 #9 (PR #244) couples reputation to PoVI-receipt + pool-royalty-held minting via `f(R)` at the ledger chokepoint (§A13). Annotation EARNING stays decoupled (AST guard green); the two paths are distinct.
3. **node-earning is no longer PARTIAL.** PR #240's closed-test harness proves the full register→stake→vouch→receipt→mint round-trip on real PG; `cmd/node` ships in the image and runs in the trial overlay (§A1, §A10).
4. **Stale config.go line citations everywhere prior to this regen** (the file grew by ~70 lines: the three new flags + their force-off-exclusion comments). All §B parse lines above are re-grep'd at `abd1572`. Package-internal citations in `internal/poolroyalty/*` and `internal/mining/pattern_mining.go` were not re-verified this pass (those files were untouched by #238–#247) and are carried forward.
5. **The two phase axes do not correspond.** The wishlist token-economy items (P1 #1–#10: two-token / PoVI / reputation-bonded / proof-of-benchmark …) are a DIFFERENT numbering from the repo's Lens-build phases (ROADMAP "Phase 2/3/…"). The repo references the token axis only obliquely as "PoVI Phases 1–5" (COORDINATION.md:14); PoVI's own internal sub-structure is "Part 1/2/3" (receipt/stake/challenge). See the chat state-capture for the verbatim phase dumps.
