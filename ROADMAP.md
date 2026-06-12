# Talyvor — Roadmap & Status

_Status board for the Talyvor build. COORDINATION.md remains the operational cross-branch seam doc; this is the strategic/status view. Last updated at main 5d52ea7 — Phase-2 BUILD COMPLETE (through LXC gating + PoVI-resolved); what remains before flip-on is not code._

## How to read this
The build runs as a relay: prompts composed, run in Claude Code, reviewed, merged. The unit of progress is a "stage" (recon → build → review → merge), not calendar time — wall-clock depends on relay cadence, not engineering size. Effort below is in stages and relative size, deliberately not dates.

## Phase 2 — LENS token economy  [BUILD COMPLETE]

Done & merged on main (all inert behind LENS_POOL_ROYALTY_MINTING_ENABLED=false):
- DISTILL — complete.
- Cache pooling 2.0 / 2.0b — cross-tenant exact + semantic pooling, three-switch consent, NUL-sentinel keyspace-disjointness leak-safety.
- 2.1 — Pool-B mint path: a served cross-tenant hit mints s×avoided_COGS LENS to the contributor, exactly-once per request_id, single-tx.
- 2.2 — realized fee-split: (1−s) margin read-surface (derived, no spend-ledger contamination) + pool_royalty in total supply.
- 2.3.0 — serve-time evidence: unsalted answer+prompt hashes per claim row, tamper-evident, no-hash⇒no-mint gate.
- 2.3b cap (primitive #1) — per-pair rolling-window mint cap, exact under concurrency, zero new locks, CI-guarded by a real-Postgres -race test.
- 2.3a — holdback/finality ledger: mint credits HELD; leader-elected unconditional sweeper finalizes held→spendable after a configurable window (72h default, trigger swappable to billing later); revoke burns from held; supply counts at FINALIZE; status-aware realized margin.
- 2.3b detection (#102) — three read-only statistical detectors (volume / self-dealing / similarity) over claim rows; never auto-acts (write-impossible at the type level — the reader's db seam is Query/QueryRow-only); flags INFORM the operator, never trigger.
- Per-entry cap (#103) — second after-credit COUNT keyed on entry_id (no new lock, no #32 surface); counts revoked (no budget refund on revoke); required hot-path index (migration 0047). Closes per-pair ≠ per-entry.
- Stage-3 revoke→adjudicate arc — revoke orchestrator (#105, per-row CAS-first held-burn; a finalized mint is NEVER revocable), read-only flag resolver (#106, candidate→held request_ids, candidates-never-verdicts), admin adjudication gate (#107, `POST /v1/admin/pool-royalty/adjudicate`; record-before-burn; the Revoker's FIRST and ONLY production caller; never-auto-act structural; migration 0048).
- Reward-loop seam test (#108) — mint → finalize → redeem proven end-to-end on real Postgres: held LXC is NOT redeemable; only finalized, spendable LENS converts to LXC.
- 2.4 / 2.5 — USD-pegged redemption / the LXC spend path: shadow (#109, OBSERVE — post-serve, void, cannot affect serving) then gating (#110, ENFORCE — pre-serve 402 block, gating-requires-shadow, fail-open). Both default-off, staged observe→enforce.

**PHASE-2 BUILD WORK IS COMPLETE.** The full loop — mint → finalize → detect → resolve → revoke → adjudicate → redeem → shadow-spend → gating — is on main, inert behind its flags (`LENS_POOL_ROYALTY_MINTING_ENABLED` / `LXCShadowSpendEnabled` / `LXCGatingEnabled`, all default-off) and real-Postgres -race CI-guarded end to end.

**U3 master economy kill-switch — DONE on main (#172).** `LENS_ECONOMY_ENABLED` (default true) is the single opt-out that runs the deployment as pure fiat SaaS: force-OFFs all 12 economy gates, unregisters the whole economy route surface (chi-native 404), gates the economy background workers, and hides the dashboard economy sections (fiat ROI/cost analytics stay). Default-true preserves byte-identical behavior. Enterprise example in `deploy/helm/lens/examples/values-enterprise.yaml`. Open: #171 (public-endpoint auth + flip default to false at external release), #173 (consent-PUT/metrics residuals).

**U18a — LXC reclassified FIAT (amends U3).** Per the U18 shape, LXC is the fiat-pegged usage credit, not token economy: the U3 force-off list is now **10 gates** (LXC gating/shadow-spend removed → they survive `LENS_ECONOMY_ENABLED=false`, so a fiat-SaaS deployment can still meter+gate paid LXC credit). `lxc/balance` is fiat (serves economy-off); `lxc/convert` stays economy-gated (it burns LENS). Follow-up #182 (fiat LXC balance dashboard panel) **DONE** — a fiat LXC credit-balance panel on the main dashboard (present economy-off; reads `GetLXCSnapshot`; peg from `economy.LXCUSDValue`).

**U18b — billing core: Stripe Checkout → LXC credit (FIAT, default off). U18 core merged PENDING live keys.** New `internal/billing` owns ALL Stripe interaction (Checkout Session create + webhook verify/handle); `DualTokenStore.CreditLXC`/`CreditLXCTx` mint LXC LENS-free (type `purchase`). `LENS_BILLING_ENABLED` (default false) gates the fiat routes INDEPENDENT of the economy master — authed `…/billing/checkout`, public Stripe-signed `…/billing/webhook` (raw-body verify), admin read-only `…/billing/purchases`. Money guarantee: #0043 claim-then-credit on `lxc_purchases(stripe_event_id)` in ONE tx + a partial unique index per session (delayed-payment async double-credit backstop); LXC recomputed server-side at the $0.10 peg (metadata never trusted); webhook returns 200 ONLY when durably recorded, else 5xx → Stripe retries. Secrets read only in `internal/config`; startup fails if enabled without `LENS_STRIPE_SECRET_KEY`/`LENS_STRIPE_WEBHOOK_SECRET`. v1 refunds = mark-only (no clawback). **Flip-on gated on entity/bank (live keys); webhook path needs HTTPS ingress.**

What remains before any flip-on is NOT code:
- External security/crypto audit (vendor + legal) of the minting/ledger path — a hard precondition on issuance; sits at the end of the Phase-2 audit, before Phase 3.
- The logged pre-flip caveats: validate connection-pool headroom under high same-workspace concurrency before enabling the LXC shadow/gating flags (the `lxc_balances` FOR UPDATE serialization point); tighten adjudication operator identity beyond the global-key default (`decided_by` is TEXT — a value change, not a schema change).

Decisions on record (resolved — kept for history):
- Poisoning snapshot decision — DECIDED: Option C (accept finality + economics). Late-discovered poisoning (a bad cached answer reported days/weeks later, after the served content has expired from cache) is NOT recovered per-mint. Rationale: the per-pair AND per-entry caps now bound a poisoned entry's exposure to at most cap × s × avoided_COGS per window on each axis (built, exact, CI-guarded), the per-serve amounts are sub-cent so a single clawback is near-symbolic, tamper-evidence (the 2.3.0 answer hash) provides adverse inference, and the holdback window still catches poisoning detected in-window. UPGRADE PATH (Option A, build only on concrete customer demand): content-addressed snapshots — persist the served bytes keyed by the existing answer_sha256 (a thousand serves of one entry dedupe to one snapshot row; claim rows already point at it), fully enabling adjudication of any mint at any time. Deferred deliberately because it is a real privacy escalation (storing response plaintext, not just digests) whose cost is only worth paying against a concrete 'prove every payout legitimate, forever' procurement requirement; gate it like none-policy (no snapshot ⇒ no mint) when built. Option B (in-window quality-judging) rejected: hot-path cost for only partial coverage.
- PoVI challenge hookup for pool mints — RESOLVED by the Stage-3 adjudication arc (detect -> resolve -> revoke -> adjudicate). The original ask ('wire the holdback's revoke path to an adjudication trigger') is met: the admin adjudication endpoint (POST /v1/admin/pool-royalty/adjudicate) records a decision and revokes the operator-chosen subset from held via AdjudicationWriter -> RevokeHeldMints -> RevokeHeldTx. Deliberate reframing: 'challenge' was scoped from an automated PoVI-style sampling challenger (which the earlier recon found uses the wrong adjudicator — povi_challenges is node/Merkle/stake-shaped — and which would VIOLATE the never-auto-act invariant) to operator-driven adjudication. The holdback window (finalize_after, 72h) is the contest window; detection/resolver only inform, the operator decides. A third-party challenger role / multi-party dispute protocol was never specified for the pool side and would be net-new design, not a hookup — out of scope unless a customer requires it.

Minting flip-on gate: supply-accounting precondition LIFTED by 2.2. Remaining: anti-gaming machinery complete (2.3 arc), business case, external audit. Minting stays inert until all land.

## Phase 3 — deepen the mineable primitives  [IN PROGRESS]

Pool-B mines ONE thing today (served cross-tenant cached responses). Phase 3 adds the others.

- **Routing-pattern CAPTURE write — DONE on main (PR #113, a9f9d1f).** The producer for the already-live routing Advisor (which read opted_in routing_patterns on the hot path but had no writer — "live consumer, dead producer"). Captures an anonymized routing observation post-serve, **structurally mint-free** (RecordPatternObservation never reaches ledger.Credit; earning is a separate stage), **double-gated + tightened**: fires iff PatternCaptureEnabled (LENS_PATTERN_CAPTURE_ENABLED, default off) AND the workspace opted in (SQL WHERE EXISTS) AND loggingPolicy≠None AND the response was SCORED (a non-200/unscored response writes nothing — no quality=0 Advisor poison). STREAMING deferred (no scored quality there). No migration (writes the existing 0023 columns, earned=0).
- **Routing-pattern EARNING — COMPLETE on main, wired + default-off** (capture #113, S1 rarity bound #116, S2 per-window cap #117, S3 idempotency claim #118 (migration 0049), S4 gated wire-up #120). Earning routes an opted-in, authenticated workspace's served request into the credit path behind `LENS_PATTERN_EARNING_ENABLED` (default off), earn-replaces-capture (one corpus row), with the full anti-gaming stack live on the path. Flip-on gates (a) serve-bound request_id (the work-product content hash), (b) authenticated workspace_id (`auth.GetAPIKey`, both auth paths), (c) serve-path error belt — all BUILD-SATISFIED by S4; remaining before the flip: (d) Sybil controls on opted-in-workspace creation + the external audit. Full detail in COORDINATION.md "Routing-pattern EARNING gate" + the CONSOLIDATED FLIP-ON CHECKLIST.
- **Alternative next stages (per the readiness map):** DISTILL-artifact royalties — the clean second: the generic Pool-B mint kernel (pool_royalty_mints / CreditHeldTx / caps / holdback / adjudication) is reusable, but the distill cache is content-addressed + anonymous, so it needs an owner-stamp + opt-in + served-reuse-event attribution layer (a Pool-B-on-the-distill-cache build). Validator-earning — greenfield + Phase-4-gated (PoVI verify is slash-only; a paid validator presupposes the independent node network and fights never-auto-act).

**DISTILL economy — decisions on record (post-#141):**
- **DISTILL cache privacy — DONE on main (S0, PR #141, 55e2f28).** Tenant-scoped the distill cache (private-by-default `wsID:` key) + a `distill_poolable` three-switch consent gate (a SEPARATE consent from cache_poolable — document-derived data is a more sensitive disclosure), closing the only ungated cross-tenant data surface in the codebase. Serve-neutral by default; nothing mints.
- **DISTILL attribution — DONE on main (S1 write, PR #148, 41f9455; S1 read, PR #160).** Durable, mint-free per-(owner, requester, artifact) counter of consented cross-tenant pooled-distill serves (`distill_serve_attribution`, migration 0052). **Read surface DONE (#160):** admin-only `GET /v1/admin/distill/attribution` (`requireAdmin`-gated) — raw rows + `?view=pairs` (the condition-(b) `SUM(serve_count)` per owner/requester probe), backed by a Query-only `Reader` kept SEPARATE from the Exec-only write `Store` (structural inertness preserved). No migration — 0052 pre-provisioned the index. The tenant-facing MASKED view (own counts only; counterparty never named, content_hash never cross-tenant) is DEFERRED (YAGNI; the masking spec stays recorded for when a product reason appears).
- **PARKED (committed): the distill-reuse royalty (the S4 mint for distill artifacts).** Reopening requires BOTH: (a) vision-OCR result caching lands — today OCR results are NOT cached (`orchestrator.go:57-62`), so the cache holds only cheap plain-text artifacts and a reuse royalty would pay for avoiding ≈0 cost; AND (b) attribution data shows material cross-tenant reuse in prod (measured primarily from `distill_serve_attribution` — serve_count per owner/requester pair; `token_events.distill_method` GROUP BY stays as the secondary consumption-side probe).
- **Rationale:** DISTILL's near-term value is privacy-gated sharing + attribution, not the mint; paying for ≈0 avoided cost creates a manufacture incentive with no surplus behind it (the S1 lesson generalized).

## WorkTier — work classification  [COMMITTED; three sequenced touchpoints]

CONCEPT: a server-side classifier that tiers the UNIT OF WORK (each served request), NOT the reward. A per-request WorkTier STRUCT of independent axis-scores — {size_bucket, cost, complexity (versioned), sensitivity} — computed post-serve in the ScoreResponse neighborhood; PLUS a separate per-workspace volume-profile AGGREGATE computed read-side over the pattern corpus (volume is a property of traffic over a window, not of a request — not a serve-path field).

Settled design decisions (recorded so they aren't re-litigated):
- **VECTOR, NOT SCALAR:** no single ordinal grade. Axes don't co-vary (short-prompt/high-complexity vs huge-doc/low-complexity) and are consumed by different layers (Advisor→complexity, LXC→cost, policy→sensitivity). A headline grade may be DERIVED for display (Track); the stored contract-level signal is the vector.
- **DESCRIPTIVE, NOT INCENTIVIZED:** nobody is paid for a tier → nothing to game. The inverse of reward-tiering, which creates manufacture incentives (the S1 lesson generalized; boundary rule in COORDINATION.md).
- **COMPLEXITY v1 = a HEURISTIC over server-observables** (input/output ratio, latency-per-token vs model baseline, reasoning/tool-use invoked, model class, retry shape), carried as a versioned field (complexity_v) so it can be upgraded behind the contract. Do NOT block the layer on a learned model.
- **SENSITIVITY:** inferred server-side or from AUTHENTICATED workspace policy, never a caller header; consumed ONLY by routing policy + capture gating; NEVER touches earning — walled off by construction (the earning path does not receive the field).
- **BUILD PATTERN:** descriptive-and-inert first (classifier computes + persists, nothing consumes), then consumers wire in one reviewed stage at a time: Advisor tier-conditional routing (the flagship payoff — quality-per-dollar SEGMENTED by work type, not averaged), the rarity tuple (a server-derived complexity bucket may join the key; S1-safe because not caller-shaped), LXC's pre-serve estimate, DISTILL's cost input, Track analytics.

The three touchpoints:
1. **NEAR-TERM (rides DISTILL):** the COST axis is needed by DISTILL's avoided-COGS attribution anyway — name/structure it there as the first WorkTier axis rather than duplicating later.
2. **PRE-FREEZE GATE ITEM:** the work_tier SCHEMA (the struct's shape) enters the external API contract BEFORE the freeze — cheap now, expensive to retrofit after the suite builds against a contract without it; the classifier behind it improves forever (freeze the interface, not the implementation). Also logged as an explicit line item at the freeze gate below, so it cannot be missed there.
3. **FULL CLASSIFIER + ADVISOR TIER-CONDITIONING:** own stage at Phase-3-tail/Phase-4 when routing intelligence is the focus.

Suite consumption: Track — spend/quality BY WORK TIER as the flagship analytics primitive; Docs/Code — tier-aware routing defaults per each product's characteristic traffic profile; frontend — tiers as the shared display vocabulary. SECONDARY: the volume-profile aggregate is a future Sybil-signal input (a fleet of workspaces with identical volume-profiles is a corroboration-fraud tell) — cross-ref the routing-pattern earning flip-on gate (d) in COORDINATION.md.

## After Phase 2 — locked order (do not reorder)
1. Phases 3–5 (largely unscoped; Phase-3 reminder: evaluate enterprise/compliance infra).
2. Full Talyvor suite — Track / Docs / Code + anything that surfaces, each to 100%. Large (~3× a single product backend minus reuse).
3. Engine to 100% + API contract FROZEN/versioned = definition of done.
4. THEN the frontend — built ONCE as a full production-grade sellable product (marketing → signup → onboarding → live dashboard → controls → audit/exports). No pilot, no demo. The single biggest discrete chunk; spans the whole suite.

OPEN QUESTION (founder decides at the gate): the locked order freezes the API BEFORE the frontend (engine-100%+freeze -> frontend). Alternative under consideration: freeze AFTER the frontend. Rationale for freezing after: the frontend is the most demanding consumer of the engine's contract; building it against a still-unfrozen engine means any contract problems it surfaces (awkward fields, missing pieces, weak error shapes) are FREE to fix, and the freeze then locks a shape proven against a real consumer rather than frozen on faith. Counterweight: the suite (built before the frontend) would then have been built against an unfrozen engine and may need contract-adjustment too — manageable, but noted. Deferred to the frontend gate.

PRE-FREEZE GATE LINE ITEM (committed — see "WorkTier" above): the work_tier SCHEMA (the per-request axis-vector struct {size_bucket, cost, complexity (versioned), sensitivity}) enters the external API contract BEFORE the freeze, whichever side of the frontend the freeze lands on. Cheap now, expensive to retrofit after the suite builds against a contract without it; freeze the INTERFACE, not the implementation — the classifier behind it improves forever. This line exists next to the freeze-timing question precisely so the item cannot be missed at the gate.

## Relative effort (stages & size, NOT dates)
- Finish Phase 2: ~5–7 more stages of the shape already done. Near, well-scoped.
- Phases 3–5: unscoped; an unknown multiple of Phase 2.
- Full suite: large.
- Frontend: major; plausibly comparable to a full product backend, spanning all products.
- Honest gestalt: "near the end of the beginning." Phase 2 is the proof-of-discipline; suite+frontend is the company-build; the parked ideas (Phase 6, Phase 7, agent-settlement-rail) are committed future work, sequenced after the frontend — not optional R&D.

## Parked ideas — APPROVED and COMMITTED future work (full reasoning in COORDINATION.md)
Deferred, NOT optional: once the engine + suite + frontend are complete, the project returns to ALL approved parked items, in priority order set by the founder. 'Parked' means sequenced-later-and-certain, not 'maybe.' Phase 6, Phase 7, the agent-settlement-rail option, and any other founder-approved idea are committed future phases.

1. Phase-6 specialized small model on Talyvor's own traffic — ML-research program, not a build task.
2. Phase-6 custom tokenizer (the legitimate "Talyvor alphabet" — more vocabulary, not fewer symbols).
3. Small-local-by-default + route-to-bigger (the honest "local brain").
4. External security/crypto audit gate before minting flip-on — a hard precondition, not a project.
5. Frontend-architecture mandates — versioned stable API + shared shell/design-system (a constraint to bake in, not a build).
6. Agent-settlement-rail strategic option — generalize the LENS ledger into an agent-to-agent settlement/controls product. Constraints: no agent is a legal principal; real fiat ⇒ money-transmission regulation; blockchain only for cross-party-distrust settlement; the moat is the controls layer, not the ledger.

Note: items 1/2/3/6 are separate-company-scale R&D, a different horizon from finishing Talyvor; 4/5 are constraints/vendor steps, near-zero build time.
