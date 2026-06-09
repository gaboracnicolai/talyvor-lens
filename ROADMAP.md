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

## After Phase 2 — locked order (do not reorder)
1. Phases 3–5 (largely unscoped; Phase-3 reminder: evaluate enterprise/compliance infra).
2. Full Talyvor suite — Track / Docs / Code + anything that surfaces, each to 100%. Large (~3× a single product backend minus reuse).
3. Engine to 100% + API contract FROZEN/versioned = definition of done.
4. THEN the frontend — built ONCE as a full production-grade sellable product (marketing → signup → onboarding → live dashboard → controls → audit/exports). No pilot, no demo. The single biggest discrete chunk; spans the whole suite.

OPEN QUESTION (founder decides at the gate): the locked order freezes the API BEFORE the frontend (engine-100%+freeze -> frontend). Alternative under consideration: freeze AFTER the frontend. Rationale for freezing after: the frontend is the most demanding consumer of the engine's contract; building it against a still-unfrozen engine means any contract problems it surfaces (awkward fields, missing pieces, weak error shapes) are FREE to fix, and the freeze then locks a shape proven against a real consumer rather than frozen on faith. Counterweight: the suite (built before the frontend) would then have been built against an unfrozen engine and may need contract-adjustment too — manageable, but noted. Deferred to the frontend gate.

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
