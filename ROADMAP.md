# Talyvor — Roadmap & Status

_Status board for the Talyvor build. COORDINATION.md remains the operational cross-branch seam doc; this is the strategic/status view. Last updated at Stage 2.3a._

## How to read this
The build runs as a relay: prompts composed, run in Claude Code, reviewed, merged. The unit of progress is a "stage" (recon → build → review → merge), not calendar time — wall-clock depends on relay cadence, not engineering size. Effort below is in stages and relative size, deliberately not dates.

## Phase 2 — LENS token economy  [IN PROGRESS]

Done & merged on main (all inert behind LENS_POOL_ROYALTY_MINTING_ENABLED=false):
- DISTILL — complete.
- Cache pooling 2.0 / 2.0b — cross-tenant exact + semantic pooling, three-switch consent, NUL-sentinel keyspace-disjointness leak-safety.
- 2.1 — Pool-B mint path: a served cross-tenant hit mints s×avoided_COGS LENS to the contributor, exactly-once per request_id, single-tx.
- 2.2 — realized fee-split: (1−s) margin read-surface (derived, no spend-ledger contamination) + pool_royalty in total supply.
- 2.3.0 — serve-time evidence: unsalted answer+prompt hashes per claim row, tamper-evident, no-hash⇒no-mint gate.
- 2.3b cap (primitive #1) — per-pair rolling-window mint cap, exact under concurrency, zero new locks, CI-guarded by a real-Postgres -race test.
- 2.3a — holdback/finality ledger: mint credits HELD; leader-elected unconditional sweeper finalizes held→spendable after a configurable window (72h default, trigger swappable to billing later); revoke burns from held; supply counts at FINALIZE; status-aware realized margin.

Remaining in Phase 2:
- 2.3b further detection — statistical concentration detectors (volume / self-dealing / similarity) over claim rows. The cap BOUNDS exposure; these FLAG patterns.
- Per-entry cap follow-up — semantic ownership churn makes per-pair ≠ per-entry.
- Poisoning snapshot decision — DECIDED: Option C (accept finality + economics). Late-discovered poisoning (a bad cached answer reported days/weeks later, after the served content has expired from cache) is NOT recovered per-mint. Rationale: the per-pair AND per-entry caps now bound a poisoned entry's exposure to at most cap × s × avoided_COGS per window on each axis (built, exact, CI-guarded), the per-serve amounts are sub-cent so a single clawback is near-symbolic, tamper-evidence (the 2.3.0 answer hash) provides adverse inference, and the holdback window still catches poisoning detected in-window. UPGRADE PATH (Option A, build only on concrete customer demand): content-addressed snapshots — persist the served bytes keyed by the existing answer_sha256 (a thousand serves of one entry dedupe to one snapshot row; claim rows already point at it), fully enabling adjudication of any mint at any time. Deferred deliberately because it is a real privacy escalation (storing response plaintext, not just digests) whose cost is only worth paying against a concrete 'prove every payout legitimate, forever' procurement requirement; gate it like none-policy (no snapshot ⇒ no mint) when built. Option B (in-window quality-judging) rejected: hot-path cost for only partial coverage.
- PoVI challenge hookup for pool mints — wire the holdback's revoke path to an adjudication trigger.
- 2.4 / 2.5 — USD-pegged redemption (the LXC spend path).
- Full Phase-2 audit, INCLUDING the external security/crypto audit of the minting/ledger path.

Minting flip-on gate: supply-accounting precondition LIFTED by 2.2. Remaining: anti-gaming machinery complete (2.3 arc), business case, external audit. Minting stays inert until all land.

## After Phase 2 — locked order (do not reorder)
1. Phases 3–5 (largely unscoped; Phase-3 reminder: evaluate enterprise/compliance infra).
2. Full Talyvor suite — Track / Docs / Code + anything that surfaces, each to 100%. Large (~3× a single product backend minus reuse).
3. Engine to 100% + API contract FROZEN/versioned = definition of done.
4. THEN the frontend — built ONCE as a full production-grade sellable product (marketing → signup → onboarding → live dashboard → controls → audit/exports). No pilot, no demo. The single biggest discrete chunk; spans the whole suite.

## Relative effort (stages & size, NOT dates)
- Finish Phase 2: ~5–7 more stages of the shape already done. Near, well-scoped.
- Phases 3–5: unscoped; an unknown multiple of Phase 2.
- Full suite: large.
- Frontend: major; plausibly comparable to a full product backend, spanning all products.
- Honest gestalt: "near the end of the beginning." Phase 2 is the proof-of-discipline; suite+frontend is the company-build; the parked R&D ideas are a later horizon.

## Parked ideas — reasoned-through, deferred (full reasoning in COORDINATION.md)
1. Phase-6 specialized small model on Talyvor's own traffic — ML-research program, not a build task.
2. Phase-6 custom tokenizer (the legitimate "Talyvor alphabet" — more vocabulary, not fewer symbols).
3. Small-local-by-default + route-to-bigger (the honest "local brain").
4. External security/crypto audit gate before minting flip-on — a hard precondition, not a project.
5. Frontend-architecture mandates — versioned stable API + shared shell/design-system (a constraint to bake in, not a build).
6. Agent-settlement-rail strategic option — generalize the LENS ledger into an agent-to-agent settlement/controls product. Constraints: no agent is a legal principal; real fiat ⇒ money-transmission regulation; blockchain only for cross-party-distrust settlement; the moat is the controls layer, not the ledger.

Note: items 1/2/3/6 are separate-company-scale R&D, a different horizon from finishing Talyvor; 4/5 are constraints/vendor steps, near-zero build time.
