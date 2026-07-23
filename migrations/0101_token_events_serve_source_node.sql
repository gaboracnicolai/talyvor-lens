-- 0101_token_events_serve_source_node.sql — CLOSE THE NODE-SERVE DENOMINATOR HOLE: make a request
-- served by a REGISTERED INFERENCE NODE countable in the cache hit rate.
--
-- WHY: a node-served request (proxy.tryNodeRouting) wrote NO token_events row — it records to the
-- learner store, not this table (that is AlertManager.RecordSpend / RecordCacheServe). So a node
-- serve was absent from BOTH the numerator AND the denominator of the cache hit rate. With node
-- routing enabled, the rate (the trial's core number — it prices BYOK and tests "our margin is the
-- cache") would read HIGH, and nothing on screen would say so. Inert today (node routing default-off,
-- no registered nodes) — which is why it is cheap to fix now, before anyone trusts the number.
--
-- serve_source = 'node' says WHO produced the served bytes: a registered node, not an upstream
-- provider and not a cache. It is NOT LIKE 'cache_hit%', so every hit-rate reader
-- (COUNT(*) FILTER (WHERE serve_source LIKE 'cache_hit%') / COUNT(*)) counts it as a MISS — no cache
-- produced the bytes — while it IS in the denominator, which is the whole point.
--
-- cost_usd on a 'node' row is TALYVOR'S PROVIDER COST = 0 (the node did the compute; Talyvor paid no
-- upstream API). What the node may be OWED is a PoVI LENS mint in lens_token_ledger — a different
-- ledger and a different unit; folding it into cost_usd here would pollute every SUM(cost_usd)
-- provider-spend total. Same discipline migration 0100 set for cache rows.
--
-- Mechanics: the 0100 CHECK enumerates the allowed serve_source values, so a new value needs the
-- constraint rebuilt. DROP IF EXISTS + ADD is re-runnable (drop-then-add always ends at the new set).
ALTER TABLE token_events DROP CONSTRAINT IF EXISTS token_events_serve_source_check;
ALTER TABLE token_events
  ADD CONSTRAINT token_events_serve_source_check CHECK (serve_source IN
    ('upstream',
     'cache_hit_exact',
     'cache_hit_semantic',
     'cache_hit_pooled',
     'cache_hit_pooled_semantic',
     'node'));
