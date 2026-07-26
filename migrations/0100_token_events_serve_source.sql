-- 0100_token_events_serve_source.sql — CACHE-SERVE SPEND VISIBILITY: make cache hits countable
-- from the SAME table every spend reader uses.
--
-- ⚠ COMMENT CORRECTED 2026-07-26 — THE ORIGINAL RATIONALE DESCRIBED A BUSINESS MODEL THAT DOES NOT
-- EXIST. It said this number "sets the BYOK subscription price and tests whether 'our margin is the
-- cache' survives real traffic". Both halves were wrong, and the DDL below was never affected — only
-- the reason given for it. Read the correction before trusting any inherited assumption here:
--
--   * THERE IS NO BYOK OFFERING AND NO SUBSCRIPTION. Bring-your-own-key is NOT BUILT, and the
--     deliberate decision is not to build it yet — it will be designed from real usage data rather
--     than from an assumption about what customers want. Nothing in this schema prices a
--     subscription, because there is no subscription to price.
--   * "OUR MARGIN IS THE CACHE" WAS THE INVERTED MODEL and has been removed from the code, the
--     README and COORDINATION.md. Billing is per-request against prepaid LXC credit at a FIXED peg
--     (1 LXC = $0.10, economy.LXCUSDValue); there is no subscription whose margin a cache hit could
--     widen. A cache hit is not retained margin on a flat fee.
--
-- WHY THIS COLUMN ACTUALLY MATTERS, which is a measurement reason and survives the correction: the
-- cache hit rate is how AVOIDED PROVIDER COGS is measured, and it is the funding basis for pooled-
-- cache contributor royalties (a mint is tied to the consumer's SETTLED charge, so an unmeasurable
-- hit is an unfundable royalty). It is a cost-and-attribution number, not a pricing input.
-- Before this migration a
-- cache-served request wrote NO token_events row (the serve branch returns before the recording
-- seam), so the single most important number in the business was unmeasurable from the table the
-- dashboard reads. The requester WAS still debited (the agent-allocator's pre-serve LXC estimate in
-- lxc_ledger); Talyvor's provider cost was zero. Billed, invisible.
--
-- serve_source says WHO produced the served bytes:
--   'upstream'            — a real provider call. Every pre-0100 row was an upstream serve, so the
--                           DEFAULT backfills history correctly.
--   'cache_hit_exact' / 'cache_hit_semantic' / 'cache_hit_pooled' / 'cache_hit_pooled_semantic'
--                         — served from Lens's cache. These rows carry cost_usd = 0.
--
-- ⚠ cost_usd ON A CACHE ROW IS TALYVOR'S PROVIDER COST, NOT WHAT THE USER PAID. The requester's
-- pre-serve LXC estimate debit stands and is deliberately not refunded — the two numbers live in
-- two ledgers (token_events = Talyvor's cost actuals; lxc_ledger = the workspace's debits) and a
-- spend UI must render them as such, never as "this request was free".
--
-- The vocabulary reuses the cache-hit metric layer labels VERBATIM (metrics.RecordCacheHit), so
-- Prometheus counters and SQL aggregates share one namespace and a hit-rate query is:
--   COUNT(*) FILTER (WHERE serve_source LIKE 'cache_hit%')::float / COUNT(*)
ALTER TABLE token_events
  ADD COLUMN serve_source TEXT NOT NULL DEFAULT 'upstream'
  CONSTRAINT token_events_serve_source_check CHECK (serve_source IN
    ('upstream',
     'cache_hit_exact',
     'cache_hit_semantic',
     'cache_hit_pooled',
     'cache_hit_pooled_semantic'));
