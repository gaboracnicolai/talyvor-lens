-- 0099_token_events_serve_source.sql — CACHE-SERVE SPEND VISIBILITY: make cache hits countable
-- from the SAME table every spend reader uses.
--
-- WHY: the closed trial's core number is the CACHE HIT RATE — it sets the BYOK subscription price
-- and tests whether "our margin is the cache" survives real traffic. Before this migration a
-- cache-served request wrote NO token_events row (the serve branch returns before the recording
-- seam), so the single most important number in the business was unmeasurable from the table the
-- dashboard reads. The requester WAS still debited (the agent-allocator's pre-serve LXC estimate in
-- lxc_ledger); Talyvor's provider cost was zero. Billed, invisible.
--
-- serve_source says WHO produced the served bytes:
--   'upstream'            — a real provider call. Every pre-0099 row was an upstream serve, so the
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
