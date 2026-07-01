# BUILD_STATE.md — Talyvor Lens canonical build-state manifest

**Single source of truth for "what is built," derived from the actual code at the SHA below — never from the roadmap, notes, or assumptions.** Regenerated (never hand-edited) whenever it goes stale.

- **Derived from main:** `21628f5`
- **Latest migration:** `0073_routing_prediction_mints.sql`
- **Config:** all `config.go:LINE` citations are `internal/config/config.go`.
- **Method:** every cell was grep'd / read from code. Where code and a note/roadmap disagree, **the code wins** — see [§C Discrepancies](#c-discrepancies-code-wins).

## Status legend
- **BUILT-&-ON** — built and active by default (no flag, or its flag/default is on).
- **BUILT-INERT** — fully built but behind a **default-off** flag (or no rows exist until one flips). The default posture.
- **PARTIAL** — built but cannot operate as-is (needs substrate that doesn't exist single-box).
- **ABSENT** — no code (or a deliberately omitted analog).

---

## The master kill-switch (read this first)

`LENS_ECONOMY_ENABLED` — **default TRUE** (`config.go` sets `c.EconomyEnabled = true`, overridden only if the env var is explicitly set). When **false**, the force-off block force-sets **13** flags false regardless of their own env:

`PatternMiningEnabled · PatternCaptureEnabled · PatternEarningEnabled · PoolRoyaltyMintingEnabled · POVIMintingEnabled · TrustfulComputeMintEnabled · CacheSharingEnabled · CachePoolableEnabled · DistillPoolableEnabled · RoutingIntelligenceEnabled · RoutingTierCohortsEnabled · EvalContributionMintingEnabled · RoutingPredictionMintingEnabled`

The **12th + 13th** are the two **Proof-of-Improvement EARNING gates** — `EvalContributionMintingEnabled` (instance 1, #250, `config.go:1268`) and `RoutingPredictionMintingEnabled` (instance 2, #260) — both MINT LENS, so both join the block. **Deliberately NOT force-offed** (documented exceptions): `LXCGatingEnabled` / `LXCShadowSpendEnabled` (fiat-pegged, not the token economy), the **U6 floor + rate cap** (safety restrictions), `WorkTierEnabled` / `GuardrailsEnabled` (non-economic), and the **measurement/routing/capability** flags — `NodeAutoRouteEnabled`, `ReputationBondedMintingEnabled`, `ProofOfBenchmarkEnabled`, `ProofOfImprovementEnabled` (anchor-selection capability, #248), `RoutingPredictionEnabled` (prediction-submission capability, #252), `RoutingPredictionScoringEnabled` (scorer/measurement, #254) — each only ever *reduces/blocks/redistributes/measures* a mint or routes traffic; none CREATES a mint, so none belongs in the force-off block. The manifest test `cmd/lens/economy_killswitch_test.go` asserts **`len(checks) == 13`** (`:69`) for the force-off set and that LXC stays wired.

> **Mint chokepoint count:** `mintTypeList` (`internal/mining/mint_gate.go`) is now **9** entries — the original 7 (`cache_mine · compute_mine · embedding_mine · annotation_mine · pattern_mine · receipt_mine_provisional · pool_royalty_held`) + **`eval_contribution_held`** (#250) + **`eval_routing_prediction_held`** (#260). `TestMintTypes_GateSet` pins all 9; `TestMintTypeList_IsSingleSource` pins `len(mintTypes)==len(mintTypeList)`. The U6 floor + 1000-LENS/24h rate cap cover every entry. Neither P-o-I mint is in `isReputationBondedType` (= `{receipt_mine_provisional, pool_royalty_held}`) — the #9 bond no-ops for both (decision c, symmetric).

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

> **#4 is REFRAMED.** Recon confirmed federated-learning-as-named has no substrate (no training loop, no gradient/aggregation, all served models external) and the only Talyvor-owned model is parked Phase-6 — so #4 is reframed as the **Proof-of-Improvement** primitive built against the best existing shared artifact. Piece 1 (§A17) = the pluggable anchor. **Piece 2 (§A18) = proof-of-eval-contribution — DONE/inert.** **Piece 3 = proof-of-routing-prediction (§A19–§A22) — PR-1/PR-2/PR-3a + the inference extraction DONE; PR-3c (real Inferer) + PR-4 (mint) pending.** An eventual Phase-6 owned-model gain would reuse the same rail.

### A18 · Proof-of-eval-contribution (P-o-I piece 2) — PR #250 (`eb2d501`)
**BUILT-INERT** · the **first real `HeldBenchmarkAnchor` caller**. Rewards a contributor for a FAIR, VALIDATED, DISCRIMINATING eval item; paid on measured discrimination through the U6/held-ledger chokepoint. Ships **INERT** (rate 0 ⇒ `NewHeldBenchmarkAnchor` refuses to construct ⇒ the minter is a total no-op even with both flags on). Migration **0069** (`benchmark_eval_items` += `author_workspace_id`/`content_hash`/`status`; `eval_contribution_mints` claim table, `item_id` UNIQUE = mint-once).
| Component | Status | file:line |
|---|---|---|
| `HeldBenchmarkAnchor` first live caller | BUILT-INERT | `internal/poolroyalty/eval_contribution_minter.go` `NewEvalContributionMinter` (the SOLE non-test `NewHeldBenchmarkAnchor` caller — `anchor_test.go` `TestHeldBenchmarkAnchor_ExactlyOneLiveCaller`) |
| Discrimination readout | BUILT | `discrimination = clamp01(4·Var(score_w over distinct UNLINKED grader workspaces))`, `MinUnlinkedGraders=3` warmup; read-only over `benchmark_probes ⋈ inference_nodes`, excluding the author's workspace + fingerprint-linked set |
| New mint type | BUILT | `TypeEvalContributionHeld = "eval_contribution_held"` → `mintTypeList` (**7→8**, `mint_gate.go` const + list only, no logic edit) |
| Earning flag (force-off) | BUILT-INERT | `LENS_EVAL_CONTRIBUTION_MINTING_ENABLED` **false**, **IN the force-off block** (`config.go:1268`); manifest **11→12**. Rate `LENS_EVAL_CONTRIBUTION_RATE_PER_POINT` **0** (inert). |
| Author exclusion (permanent) | BUILT | the author is excluded from drawing (`DrawItem`), grading, and earning on their own items (reuses #250's `author_workspace_id` + `workspace_card_fingerprints` self-deal join) |

> NO-LOOP: the score producer (`benchprobe`/`eval.StaticScore`) is import-guarded mint-free; the minter reads `benchmark_probes` and writes only the ledger + claim table. **Residual (logged pre-public-mint gate):** different-card/no-card sockpuppet evades the fingerprint link — bounded by `MinUnlinkedGraders` + the U6 24h author cap, not eliminated.

### A19 · Routing-prediction unit (P-o-I piece 3, PR-1) — PR #252 (`69a9dfb`)
**BUILT-INERT** · the attributable prediction object recon R1 found missing (routing_patterns are anonymized post-serve OBSERVATIONS, not predictions). Migration **0070** `routing_predictions(workspace_id, feature_category, input_token_range, complexity_bucket, model, provider, status)`. **One LIVE prediction per (workspace, cohort)** via a partial `UNIQUE … WHERE status IN ('pending','active')` (anti-hedge-farm; retire frees the slot). Pure-CRUD `internal/routingpredict/` (import-guarded: no `internal/routing`/mining/anchor/ledger). `LENS_ROUTING_PREDICTION_ENABLED` **false** — a CAPABILITY flag gating SUBMISSION (table stays provably empty until on), **NOT** force-off (no mint). Operator tool `cmd/lens-routeseed`. NO mint, NO anchor caller.

### A20 · Cohort-tag the held eval pool (P-o-I piece 3, PR-2) — PR #253 (`8187697`)
**BUILT** · gives a prediction's "cohort C" a resolvable held slice (recon R2). Migration **0071** adds `feature_category`/`input_token_range`/`complexity_bucket` to `benchmark_eval_items` (nullable). Only **two** dims are pure-input derivations — `internal/cohort/DeriveInputCohort` composes the EXACT exported serve-path funcs (`mining.InputBucketFor` + `worktier.ComplexityBucketFor(router.AnalyseComplexity)`), pinned by a golden parity test (no `proxy` edit). `feature_category` is **client-declared** (the `X-Talyvor-Feature` header), so it's supplied at seed, not derived. `cmd/lens-benchseed --backfill` re-derives the two derived dims; node-blind preserved (the cohort tag never enters the probe payload). NO mint.

### A21 · Routing-prediction scorer (P-o-I piece 3, PR-3a) — PR #254 (`1bfb807`)
**BUILT-INERT (provably — no real inference backend wired)** · scores a prediction skill-above-baseline. Migration **0072** `routing_prediction_scores(prediction_id UNIQUE [score-once], slice_size, m_avg, baseline_avg, baseline_model, skill_margin)`. `internal/routingscore/` sweeper behind an **`Inferer` interface `{ Infer(ctx, model, input) (string,error) }`** with a **FAKE impl only** — `main.go` injects a **nil Inferer** ⇒ `RunOnce` no-ops even flag-on, until PR-3c. Baseline = `Advisor.RecommendByRange(cohort)` (skip if `BasisNone`); `skill_margin = clamp01(avg(M)−avg(baseline))`; the predictor's authored + fingerprint-linked items are excluded from the slice. Bounds: `MinSliceSize=3`, `SliceCap=20`, `BatchLimit=20`. `LENS_ROUTING_PREDICTION_SCORING_ENABLED` **false** — capability/measurement, **NOT** force-off. Import-guard: no `internal/proxy`, no `output_quality`, no mint/anchor symbol (reads held `StaticScore` only). The scorer is mint-free (the mint is §A23); at PR-3a it had a nil Inferer — the **real provider-backed Inferer arrived in PR-3c** (§A22), the scorer staying inert-by-default behind the scoring flag.

### A22 · `internal/inference` — the provider core (P-o-I piece 3, PR-3b A′ #256 → PR-3c #259 `9c7805e`)
**BUILT — behavior-preserving; `internal/inference` now OWNS the full provider-calling core.** Done in two steps: #256 (`071d061`, A′) moved the round-trip + leaf helpers; #259 (`9c7805e`, the full (i) type move) moved the rest. The package (cycle-free — imports `catalog` + `retry` + stdlib, **NO `internal/proxy`**) holds: the **`ProviderConfig` type** (unexported fields) + the **`Provider` interface** + **`ConfigFor(name, Endpoints)`** (the moved `configForProvider` switch, `Endpoints` by value — bedrock snapshot preserved) + **`ConfigForKey(name, Endpoints, key)`** (the moved `applyKey` credential override) + **`RunUpstream`** (the `forward` round-trip VERBATIM) + the gemini/bedrock translate+sign + `Usage`/`ExtractUsage` + the **real provider-backed `Inferer`** (`inferer.go`: `ProviderInferer` resolves model→provider via `catalog.Get`, then `ConfigFor`→`RunUpstream`→`ParseResponse`→first-choice extraction). Files: `providerconfig.go · config.go · runupstream.go · gemini.go · bedrock.go · usage.go · inferer.go`.
- **proxy delegates** (no behavior change): `type providerConfig = inference.ProviderConfig` alias keeps type-name refs compiling; ~49 `cfg.name`→`cfg.ProviderName()`; the 3 inline handler literals (`HandleOpenAI/Anthropic/Google`)→`p.configForProvider`; `forwardWithFallback` translate block→`BuildRequest`/`ParseResponse`; `forward`→`inference.RunUpstream`; `applyKey`→`inference.ConfigForKey`; `provider.go` **DELETED**; `Usage`/`BedrockConfig` aliases stay.
- **The Inferer is wired but the scorer stays inert-by-default:** `main.go` injects `inference.NewProviderInferer(...)` into `routingscore.NewScorer` (replacing the nil); `RunOnce` is still gated by `LENS_ROUTING_PREDICTION_SCORING_ENABLED` (capability built, **not armed**). `routingscore` stays provider-agnostic (Inferer by interface; no proxy/inference/catalog import).

### A23 · Proof-of-routing-prediction MINT (P-o-I instance 2, PR-4) — PR #260 (`21628f5`)
**BUILT-INERT** · the **SECOND live `HeldBenchmarkAnchor` caller** (P-o-I instance 2). Pays a contributor whose routing prediction was proven skill-above-baseline on the held eval slice, on the measured `skill_margin`, through the SAME held-ledger / U6 chokepoint. Migration **0073** `routing_prediction_mints` (generic finalize columns: `request_id` PK = the score's `prediction_id` = mint-once; `contributor_workspace_id` = the payee = `routing_predictions.workspace_id` — so the existing generic `FinalizeSweeper` settles it UNCHANGED).
| Component | Status | file:line |
|---|---|---|
| `RoutingPredictionMinter` (2nd anchor caller) | BUILT-INERT | `internal/poolroyalty/routing_prediction_minter.go` — reads `routing_prediction_scores.skill_margin` ⋈ `routing_predictions.workspace_id` by SQL; `amount = ratePerPoint × clamp01(skill_margin)`; `ON CONFLICT (request_id) DO NOTHING` + `RowsAffected==0`; `CreditHeldTx` |
| New mint type | BUILT | `TypeRoutingPredictionHeld = "eval_routing_prediction_held"` → `mintTypeList` (**8→9**). NOT reputation-bonded (decision c — symmetric with eval-contribution) |
| Earning flag (force-off) | BUILT-INERT | `LENS_ROUTING_PREDICTION_MINTING_ENABLED` **false**, **IN the force-off block**; manifest **12→13**. Rate `LENS_ROUTING_PREDICTION_RATE_PER_POINT` **0** (inert) |
| Reachability guard | BUILT | flipped **"exactly one"→"exactly two"** (`anchor_test.go`): the two sanctioned callers are `eval_contribution_minter.go` + `routing_prediction_minter.go` |
| NO-LOOP import-guard | BUILT | `routing_prediction_noloop_test.go` — the minter forbids `routingscore`/`routing`/`proxy`/`inference` (the feedback path) + writes no scores/weights table; ALLOWS `mining` (the ledger) |

> NO-LOOP: the minter READS scores by SQL, WRITES only the ledger + `routing_prediction_mints`; minting LENS cannot change a future `skill_margin` (the score is `eval.StaticScore(M)` vs the Advisor baseline — neither reads the ledger). The **#254 author-exclusion sits two layers upstream** (`scorer.cohortSlice` → `insertScore` → this claim), so a self-dealt score never reaches the table. **INERT:** rate 0 ⇒ nil anchor ⇒ `RunOnce` no-op (zero DB); live mint needs BOTH the force-off flag lifted AND a positive rate — merging armed nothing.

### A24 · Test-infra (not economy surface)
- **CI race fix — PR #251 (`235bc11`, test-only):** a pre-existing public-schema DDL collision under `go test ./...` at default parallelism (`-race`), fixed AT SOURCE via a **per-package private-schema `TestMain`** (advisory-locked). **Requirement going forward:** every new DB-touching package needs its own `schema_isolation_test.go` `TestMain` (the routing-prediction minter integration test uses its own `search_path = lens_routingpredmint_test`).
- **THE PROVIDER-SEAM ORACLE SET (3) + the inference pin — UNTOUCHABLE:** `forward_authheaders_test.go` + `forward_retry_test.go` (#255 `dbbbd6c` — header/auth ordering + retry) and `applykey_characterization_test.go` (#258 `3b30490` — the four-branch credential map: openai `Bearer`, anthropic `x-api-key`+`anthropic-version:2023-06-01`, google `?key=` in the URL, mistral/groq/vllm/bedrock pass-through). They gated the #259 type move byte-identical (the #258 oracle took an observation-surface-only edit: `setAuth→ApplyAuth`, `upstreamURLFn→UpstreamURL`; assertions unchanged). The **inference-side `ConfigForKey` characterization pin** (`internal/inference/configforkey_characterization_test.go`) pins the SAME credential map on the logic owner — the durable tripwire.

> **P-o-I piece-3 arc — COMPLETE:** PR-1 unit (§A19, #252) · PR-2 cohort tags (§A20, #253) · PR-3a scorer (§A21, #254) · forward oracle (#255) · PR-3b A′ extraction (§A22, #256) · applyKey oracle (#258) · PR-3c full type move + real Inferer (§A22, #259) · **PR-4 the mint (§A23, #260)** — all DONE. **The Proof-of-Improvement primitive now has TWO mints** — eval-contribution (§A18, #250) + routing-prediction (§A23, #260) — **both BUILT-INERT, both unbonded**, both routed through the U6 chokepoint, both gated behind a force-off flag + a default-0 rate. The held-benchmark anchor has exactly **two** live callers.

### A25 · P2 (wishlist "Intelligence") closure — recon findings (this session, read-only)
**P2 is BUILD-COMPLETE EXCEPT the deferred #5 — not "complete."** Per-item true state: **#4-reframe DONE** (the Proof-of-Improvement primitive; #2 proof-of-savings = instance-1) · **pluggable anchor DONE** (§A17, #248) · **proof-of-eval-contribution DONE/inert** (§A18, #250) · **proof-of-routing-prediction DONE/inert** (§A19–§A23, #252→#260) · **#8 SATISFIED by #250** · **#5 DEFERRED-BLOCKED**. Every *buildable* P2 item is built; the one un-built item (#5) is correctly deferred on two undesigned substrates. **Do NOT call P2 "complete" — it is "build-complete except the deferred #5."**

> **#8 proof-of-dataset-contribution — SATISFIED BY #250 (no separate mint).** Recon established #8-as-named is already built as proof-of-eval-contribution (#250): the "dataset" a contributor adds to in this system **is the held eval pool** (`benchmark_eval_items` — verifier-private, shared, author-attributed via mig 0069, discrimination-scored, author-excluded), and "proof-of-dataset-contribution" is the same mechanism #250 already mints. Every other reading FAILS a four-test: a training-corpus reading → **no owned-model consumer** (the #4 dead end, Phase-6 prose); a coverage/novelty-scoring reading → **diffuse attribution** (candidate-C) + double-pays #250; and the one "dataset"-named table `eval_datasets` (mig 0030) is a **PRIVATE per-workspace regression fixture**, not a shared contribution surface (trivially self-farmable). **Verdict: #8 = #250, done — do not re-investigate.**

> **#5 demand-response compute — DEFERRED, blocked on two undesigned substrates.** Recon established #5 is a **demand-SIDE pricing/incentive** mechanism (reward shifting/reducing compute demand under system stress) — NOT a supply-side contribution mint, so it does **not** fit the `HeldBenchmarkAnchor` template. It is not buildable in any non-placeholder form now because BOTH substrates are absent AND undesigned: **(a) NO congestion/load signal** — load indicators exist fragmentarily (`lens_inflight_requests` gauge, per-workspace rate-limit remaining, circuit-breaker state, least-loaded routing) but there is **no unified congestion readout and nothing manages/prices global load** (`MaxConcurrent` is stored-not-enforced; no overload-shed); **(b) NO price lever** — the entire spend side is **shadow-only/inert** (`LXCShadowSpendEnabled`/`LXCGatingEnabled`/`BillingEnabled` all default-false; flat catalog pricing), gated behind the public-mint go-live (audit/legal/Stripe). A demand-response incentive modulates a *price* under a *stress signal*; neither exists. A dormant hook now would be an empty placeholder (reads nothing, writes nothing) that auto-arms into a no-op or gets rewritten against the real substrate anyway — **negative value.** **Correct status: deferred until (a) the pricing substrate is live AND (b) a demand-stress signal is designed**; then #5 is built properly and ships inert/auto-arming like the other mints. Founder confirmed no congestion-signal or price-lever definition exists yet.

---

## §B — Every economy / feature flag: default + force-off membership (current lines)

All booleans are `parseBoolEnv` (**false** when unset) unless noted; **force-false** by `LENS_ECONOMY_ENABLED=false` unless marked **(exempt)**.

| Flag | Default | parse line | In force-off block? | Flipping ON does… |
|---|---|---|---|---|
| `LENS_ECONOMY_ENABLED` | **TRUE** | set in `Load()` | n/a (the block itself) | Master switch; **false** force-offs the 12 gates below + unregisters the economy route surface. |
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
| `LENS_EVAL_CONTRIBUTION_MINTING_ENABLED` | false | field :448, force-off `:1268` | **yes** | The proof-of-eval-contribution EARNING gate (§A18). MINTS LENS ⇒ in the force-off block (the 12th). Needs a positive rate too. |
| `LENS_ROUTING_PREDICTION_MINTING_ENABLED` | false | field, force-off block | **yes** | The proof-of-routing-prediction EARNING gate (§A23, #260). MINTS LENS ⇒ in the force-off block (the **13th**). Needs a positive rate too. |
| `LENS_NODE_AUTOROUTE_ENABLED` | false | :677 | **no** | Gateway auto-route to a registered node (§A16). Routing, not a mint. |
| `LENS_REPUTATION_BONDED_MINTING_ENABLED` | false | :678 | **no** | `f(R)` bond on PoVI/royalty mints (§A13). Reduces/blocks, never enables. |
| `LENS_PROOF_OF_BENCHMARK_ENABLED` | false | :679 | **no** | Probe scheduler + quality routing bias + probe-mint suppression (§A14). Measurement/routing. |
| `LENS_PROOF_OF_IMPROVEMENT_ENABLED` | false | field :440 | **no** | Capability to SELECT the held-benchmark anchor; now has a reachable caller (the §A18 eval-contribution mint, gated by the earning flag above). Capability, cannot outrun U6. |
| `LENS_ROUTING_PREDICTION_ENABLED` | false | field :456 | **no** | Capability gating routing-PREDICTION submission (§A19). Inert data substrate, no mint. |
| `LENS_ROUTING_PREDICTION_SCORING_ENABLED` | false | field :464 | **no** | Capability/measurement gating the routing-prediction SCORER sweep (§A21). Produces a score, mints nothing. The real provider-backed Inferer is now wired (#259), but the scorer stays inert until this flag flips (capability built, not armed). |
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
| `LENS_EVAL_CONTRIBUTION_RATE_PER_POINT` | **0** (inert) | set :877 | LENS-per-discrimination-point for the §A18 mint. 0 ⇒ `NewHeldBenchmarkAnchor` refuses ⇒ minter no-ops. A deliberate later flip. |
| `LENS_ROUTING_PREDICTION_RATE_PER_POINT` | **0** (inert) | parsed in `Load()` | LENS-per-skill-margin-point for the §A23 mint. 0 ⇒ `NewHeldBenchmarkAnchor` refuses ⇒ minter no-ops. A deliberate later flip. |

---

## §C — Discrepancies (code wins)

1. **Force-off set is 12** (verified `economy_killswitch_test.go` `len(checks) == 12`). The 12 are the original 11 (pattern trio, pool-royalty, povi, trustful-compute, cache-sharing/poolable, distill-poolable, routing-intelligence, tier-cohorts) + `EvalContributionMintingEnabled` (#250 — it MINTS). The capability/measurement flags (`NodeAutoRoute`, `ReputationBondedMinting`, `ProofOfBenchmark`, `ProofOfImprovement`, `RoutingPrediction`, `RoutingPredictionScoring`) are DELIBERATELY excluded — each routes / reduces / suppresses / measures, never creates a mint.
2. **`mintTypeList` is 9** (verified — `mint_gate.go` + `TestMintTypes_GateSet`): the original 7 + `eval_contribution_held` (#250) + `eval_routing_prediction_held` (#260). Both P-o-I mints are **absent from `isReputationBondedType`** (= `{receipt_mine_provisional, pool_royalty_held}`) — decision c, the #9 bond no-ops for both.
3. **Reputation is no longer purely money-decoupled.** P1 #9 (PR #244) couples reputation to PoVI-receipt + pool-royalty-held minting via `f(R)` at the ledger chokepoint (§A13). Annotation EARNING stays decoupled (AST guard green); the two paths are distinct.
4. **node-earning is no longer PARTIAL.** PR #240's closed-test harness proves the full register→stake→vouch→receipt→mint round-trip on real PG; `cmd/node` ships in the image and runs in the trial overlay (§A1, §A10).
5. **config.go line citations: §A1–§A17 + the §B numeric knobs are CARRIED FORWARD from the `c4742ab` regen and NOT re-verified this pass** (config.go has grown by ~hundreds of lines since; older `:NNN` cites are approximate; the subsystems are unchanged). The structural counts — manifest **13**, `mintTypeList` **9**, latest migration **0073**, **HeldBenchmarkAnchor live callers = 2** (`eval_contribution_minter.go` + `routing_prediction_minter.go`; `anchor.go` is the definition) — are VERIFIED at `21628f5`, not transcribed.
6. **`internal/inference` now OWNS the provider core** (§A22): the `ProviderConfig` type + `Provider` interface + `ConfigFor` + `ConfigForKey` + `RunUpstream` + gemini/bedrock translate-sign + `Usage`/`ExtractUsage` + the real `ProviderInferer` — cycle-free (imports `catalog`+`retry`+stdlib, no `internal/proxy`). Proxy delegates via the `type providerConfig = inference.ProviderConfig` alias. Other arc packages (all import-guarded mint-free): `internal/routingpredict` (§A19), `internal/cohort` (§A20, no DB), `internal/routingscore` (§A21, provider-agnostic). The provider seam is guarded by **3 untouchable oracles** (2× #255 forward + #258 applyKey) + the inference-side `ConfigForKey` pin (§A24).
7. **The phase axes do not correspond — the wishlist axis is FOUNDER-DEFINED, not repo-tracked.** The founder's wishlist token-economy grouping (P1 Foundation / P2 Intelligence / P3 …, over items #1–#10) is a DIFFERENT numbering from the repo's own phases and **is not enumerated in the repo** (recon this session). Do not conflate it with: the repo's `ROADMAP.md` "Phase 3 — deepen the mineable primitives" (`:44`, a DIFFERENT axis, **stale** — dated `5d52ea7`, pre-P-o-I-arc — and largely already built); COORDINATION.md's "Phase 3 = routing-pattern earning" (`:77`, done); or "PoVI Phases 1–5" (COORDINATION.md:14, never enumerated; PoVI's own sub-structure is "Part 1/2/3"). **Wishlist axis status:** P1 (Foundation) done · P2 (Intelligence) build-complete-except-#5 (§A25) · **P3 (from the founder's roadmap, NOT the repo) = #6 proof-of-latency-locality, #7 carbon-verified green mining, F2 idle-mind network (verified/opt-in/embeddings-only)** — the next recon target is **#6**.

### Logged residuals + pre-public-mint gates (carried)
- **Different-card / no-card sockpuppet** (§A18 eval-contribution + the routing-prediction author exclusion): evades the `workspace_card_fingerprints` link (default-allow-on-missing); bounded by `MinUnlinkedGraders`/`MinSliceSize` + the U6 24h author cap, NOT eliminated.
- **Gateway-bound `request_id`** (§A14 #10): the receipt `request_id` is node-asserted; the probe-mint suppression is honest-node-robust but a malicious node can bypass via a non-probe id — the pre-existing receipt-fabrication surface. It is **also the one residual farm vector for the routing-prediction mint** (§A23): manufacturing a weak baseline via fabricated receipts corrupts `Advisor.Recommend(cohort)`, inflating `skill_margin`. Tracked as a pre-public-mint gate.
- **Reputation `SUM(delta)` materialization** (§A13): the per-mint fold is O(events-per-workspace); a materialized current-R is a logged follow-up.

> **Pre-public-mint GO-LIVE gates (both P-o-I mints stay INERT until ALL clear):** the gateway-bound `request_id` receipt-trust gate (above) + the different-card sockpuppet bound + audit + legal + live Stripe + the deliberate flip (lift the force-off flag AND set a positive rate). Merging the mint PRs (#250, #260) built the mints; it armed nothing.
